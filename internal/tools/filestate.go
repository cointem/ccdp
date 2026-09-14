package tools

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// File freshness tracking (Claude Code's readFileState idea): Read records the
// file's mtime at read time; Edit/Write verify it before mutating. A file that
// changed on disk after the last Read is rejected so the agent never
// overwrites unseen changes. The state is owned by a Resources value, rather
// than by the tools package.
type fileReadEntry struct {
	mtime    time.Time
	size     int64
	dev      uint64
	inode    uint64
	digest   [sha256.Size]byte
	complete bool
}

// FileReadSnapshot is captured from the same open descriptor as Read's bytes.
// Keeping the digest with that descriptor avoids recording a later file state
// than the content the model actually observed.
type FileReadSnapshot struct {
	Mtime    time.Time
	Size     int64
	Dev      uint64
	Inode    uint64
	Digest   [sha256.Size]byte
	Complete bool
}

// FileReadRecord is the observable state of one tracked path.
type FileReadRecord struct {
	Path   string
	Mtime  time.Time
	Digest string
}

// FileState owns read/freshness records for one resource scope.
type FileState struct {
	owner string

	mu sync.Mutex
	// writeMu serializes freshness-check + mutation sequences performed by
	// built-in Write/Edit tools. The OS flock in file.go protects the file
	// descriptors too; this lock closes races between two tool goroutines that
	// share one session resource.
	writeMu sync.Mutex
	paths   map[string]fileReadEntry
	order   []string // read order, oldest first (most recently read last)
	closed  bool
}

// NewFileState creates an empty freshness tracker for owner.
func NewFileState(owner string) *FileState {
	return &FileState{owner: owner, paths: map[string]fileReadEntry{}}
}

// Owner returns the immutable scope identifier.
func (s *FileState) Owner() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// CheckOpen verifies that this state can still be used.
func (s *FileState) CheckOpen() error {
	if s == nil {
		return fmt.Errorf("file safety: nil state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("file safety: state for %q is closed", s.owner)
	}
	return nil
}

// Close invalidates this tracker. Records are memory-only and are discarded.
func (s *FileState) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.paths = map[string]fileReadEntry{}
	s.order = nil
	s.mu.Unlock()
}

// MarkFileRead records the observed state of a path after a successful Read.
func (s *FileState) MarkFileRead(path string) {
	if s == nil || path == "" {
		return
	}
	snapshot, err := fileSnapshot(path)
	if err != nil {
		return
	}
	s.recordRead(path, snapshot)
}

// MarkFileReadSnapshot records a snapshot captured by the caller's read
// descriptor. It is the preferred method for Read because a second stat/hash
// after closing that descriptor could otherwise observe a concurrent writer.
func (s *FileState) MarkFileReadSnapshot(path string, snapshot FileReadSnapshot) {
	if s == nil || path == "" {
		return
	}
	s.recordRead(path, snapshot)
}

func (s *FileState) recordRead(path string, snapshot FileReadSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.paths == nil {
		s.paths = map[string]fileReadEntry{}
	}
	s.paths[path] = fileReadEntry{mtime: snapshot.Mtime, size: snapshot.Size,
		dev: snapshot.Dev, inode: snapshot.Inode, digest: snapshot.Digest, complete: snapshot.Complete}
	// Move (or append) the path to the end of the recency order.
	for i, p := range s.order {
		if p == path {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, path)
}

// RecentReads returns up to n most recently read paths, newest first.
func (s *FileState) RecentReads(n int) []FileReadRecord {
	if s == nil || n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var out []FileReadRecord
	for i := len(s.order) - 1; i >= 0 && len(out) < n; i-- {
		p := s.order[i]
		if e, ok := s.paths[p]; ok {
			out = append(out, FileReadRecord{Path: p, Mtime: e.mtime, Digest: fmt.Sprintf("%x", e.digest[:])})
		}
	}
	return out
}

// CheckFileFresh validates that path may be mutated now. It returns an error
// when the file was never read in this owner or changed since its last Read.
func (s *FileState) CheckFileFresh(path string) error {
	if s == nil {
		return fmt.Errorf("file safety: nil state")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("file safety: state for %q is closed", s.owner)
	}
	entry, ok := s.paths[path]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("file safety: %s has not been read yet — Read it before editing so no unseen changes are overwritten", path)
	}
	if !entry.complete {
		return fmt.Errorf("file safety: %s was only partially read — read the complete file before editing or overwriting it", path)
	}
	// Open once and compare the cheap descriptor metadata before hashing. In
	// particular, a small file that was replaced by a huge file must be
	// rejected without reading the huge replacement. The bounded fingerprint
	// below still catches same-size/mtime restoration attacks.
	snapshot, err := fileFreshSnapshot(path, entry)
	if err != nil {
		return fmt.Errorf("file safety: %s could not be fingerprinted: %w", path, err)
	}
	if !snapshot.Mtime.Equal(entry.mtime) || snapshot.Size != entry.size ||
		snapshot.Dev != entry.dev || snapshot.Inode != entry.inode {
		return fmt.Errorf("file safety: %s has been modified since it was last read (by the user or another process) — Read it again to pick up the current content before editing", path)
	}
	if snapshot.Digest != entry.digest {
		return fmt.Errorf("file safety: %s content changed since it was last read (even if its timestamp/size were restored) — Read it again before editing", path)
	}
	// Do not hold the state mutex during filesystem I/O, but make sure the
	// record we checked is still the current one before allowing a mutation.
	// A concurrent Read or Close must force the caller to establish a fresh
	// admission boundary rather than silently validating against an old entry.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("file safety: state for %q is closed", s.owner)
	}
	current, ok := s.paths[path]
	if !ok || current != entry {
		return fmt.Errorf("file safety: %s read state changed while it was being checked — Read it again before editing", path)
	}
	return nil
}

// fileDigest computes a constant-memory content fingerprint. It is checked
// after the cheap stat comparison so ordinary unchanged edits avoid this I/O;
// when a caller restores mtime/size, the digest still detects the change.
func fileDigest(path string) ([sha256.Size]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer f.Close()
	return fileDigestOpen(f)
}

func fileDigestOpen(f *os.File) ([sha256.Size]byte, error) {
	if f == nil {
		return [sha256.Size]byte{}, fmt.Errorf("nil file")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

// fileDigestOpenLimit fingerprints at most limit bytes from an already-open
// descriptor. A freshness check supplies expectedSize+1: the extra byte lets
// the post-read descriptor stat identify growth while ensuring the hash itself
// cannot run away on a replacement that is much larger than expected.
func fileDigestOpenLimit(f *os.File, limit int64) ([sha256.Size]byte, error) {
	if f == nil {
		return [sha256.Size]byte{}, fmt.Errorf("nil file")
	}
	if limit < 0 {
		return [sha256.Size]byte{}, fmt.Errorf("negative digest limit")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	if limit > 0 {
		if _, err := io.CopyN(h, f, limit); err != nil && err != io.EOF {
			return [sha256.Size]byte{}, err
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

// fileSnapshot fingerprints one open descriptor while holding a shared
// advisory lock. A size/mtime change during hashing is retried once so a
// reader never records an obviously torn file state.
func fileSnapshot(path string) (FileReadSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileReadSnapshot{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return FileReadSnapshot{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fileSnapshotOpen(f)
}

func fileSnapshotOpen(f *os.File) (FileReadSnapshot, error) {
	if f == nil {
		return FileReadSnapshot{}, fmt.Errorf("nil file")
	}
	for attempt := 0; attempt < 2; attempt++ {
		info, err := f.Stat()
		if err != nil {
			return FileReadSnapshot{}, err
		}
		digest, err := fileDigestOpen(f)
		if err != nil {
			return FileReadSnapshot{}, err
		}
		after, err := f.Stat()
		if err != nil {
			return FileReadSnapshot{}, err
		}
		dev, inode := fileIdentity(info)
		afterDev, afterInode := fileIdentity(after)
		if info.ModTime().Equal(after.ModTime()) && info.Size() == after.Size() &&
			dev == afterDev && inode == afterInode {
			return FileReadSnapshot{Mtime: after.ModTime(), Size: after.Size(), Dev: afterDev, Inode: afterInode, Digest: digest, Complete: true}, nil
		}
	}
	return FileReadSnapshot{}, fmt.Errorf("file changed while it was being fingerprinted")
}

// fileFreshSnapshot performs the freshness fingerprint against one open file
// descriptor. The first stat is deliberately before hashing so a changed size
// or identity is rejected without scanning the replacement. The second stat
// closes the ordinary read-while-growing race; the digest comparison in
// CheckFileFresh remains the defense for same-size content changes whose
// timestamp was restored.
func fileFreshSnapshot(path string, entry fileReadEntry) (FileReadSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileReadSnapshot{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return FileReadSnapshot{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	before, err := f.Stat()
	if err != nil {
		return FileReadSnapshot{}, err
	}
	beforeSnapshot := fileReadMetadata(before)
	if !fileMetadataMatchesEntry(beforeSnapshot, entry) {
		return beforeSnapshot, nil
	}

	limit := entry.size
	if limit < int64(^uint64(0)>>1) {
		limit++
	}
	digest, err := fileDigestOpenLimit(f, limit)
	if err != nil {
		return FileReadSnapshot{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return FileReadSnapshot{}, err
	}
	afterSnapshot := fileReadMetadata(after)
	if !sameFileMetadata(beforeSnapshot, afterSnapshot) {
		return afterSnapshot, nil
	}
	afterSnapshot.Digest = digest
	afterSnapshot.Complete = true
	return afterSnapshot, nil
}

func fileReadMetadata(info os.FileInfo) FileReadSnapshot {
	if info == nil {
		return FileReadSnapshot{}
	}
	dev, inode := fileIdentity(info)
	return FileReadSnapshot{Mtime: info.ModTime(), Size: info.Size(), Dev: dev, Inode: inode}
}

func fileMetadataMatchesEntry(snapshot FileReadSnapshot, entry fileReadEntry) bool {
	return snapshot.Mtime.Equal(entry.mtime) && snapshot.Size == entry.size &&
		snapshot.Dev == entry.dev && snapshot.Inode == entry.inode
}

func sameFileMetadata(a, b FileReadSnapshot) bool {
	return a.Mtime.Equal(b.Mtime) && a.Size == b.Size && a.Dev == b.Dev && a.Inode == b.Inode
}

// fileIdentity returns the stable device/inode pair when the host exposes it.
// mtime and size remain part of the fingerprint because some filesystems do
// not provide useful Stat_t identity fields (and because content checks still
// catch same-inode rewrites).
func fileIdentity(info os.FileInfo) (dev, inode uint64) {
	if info == nil {
		return 0, 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}

// ForgetFile drops the freshness record for a path.
func (s *FileState) ForgetFile(path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paths, path)
	for i, p := range s.order {
		if p == path {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Clear removes all records while retaining the owner.
func (s *FileState) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.paths = map[string]fileReadEntry{}
	s.order = nil
	s.mu.Unlock()
}
