package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"ccdp/internal/atomicfile"
)

const blobsDirName = "blobs"

// ArtifactStore publishes immutable content-addressed bytes inside one
// session.  PutReader syncs the staged file, renames it into blobs/, and
// syncs that directory before returning a BlobRef.  An event referring to the
// ref can therefore be committed only after the bytes are durable.
type ArtifactStore struct {
	dir      string
	maxBytes int64
	syncFile atomicfile.SyncFile
	syncDir  func(string) error
	readOnly bool
}

// NewArtifactStore creates a scoped artifact store.  It creates the blobs
// directory only when writable; read-only instances never mutate the tree.
func NewArtifactStore(sessionDir string, options ...JSONLOption) (*ArtifactStore, error) {
	if sessionDir == "" {
		return nil, errors.New("session: artifact session directory is required")
	}
	opts := applyJSONLOptions(options)
	blobsDir := filepath.Join(sessionDir, blobsDirName)
	existingAncestor := nearestExistingDirectory(filepath.Dir(blobsDir))
	if err := os.MkdirAll(blobsDir, 0o700); err != nil {
		return nil, err
	}
	if err := syncCreatedDirectories(blobsDir, existingAncestor, opts.SyncDir); err != nil {
		return nil, err
	}
	return &ArtifactStore{dir: blobsDir, maxBytes: opts.MaxBlobBytes, syncFile: opts.SyncFile, syncDir: opts.SyncDir}, nil
}

// OpenArtifactStoreReadOnly opens a store without creating its blobs
// directory.  It is suitable for replay and export paths.
func OpenArtifactStoreReadOnly(sessionDir string, options ...JSONLOption) *ArtifactStore {
	opts := applyJSONLOptions(options)
	return &ArtifactStore{dir: filepath.Join(sessionDir, blobsDirName), maxBytes: opts.MaxBlobBytes, syncFile: opts.SyncFile, syncDir: opts.SyncDir, readOnly: true}
}

func (s *ArtifactStore) path(ref BlobRef) (string, error) {
	if s == nil || filepath.Clean(s.dir) == "." || s.dir == "" {
		return "", errors.New("session: artifact directory is required")
	}
	if err := ref.validate(); err != nil {
		return "", err
	}
	// SHA-256 is hex and fixed width, but cleanly re-check the path boundary so
	// future BlobRef changes cannot introduce traversal through this function.
	path := filepath.Join(s.dir, ref.Hash)
	if filepath.Dir(path) != filepath.Clean(s.dir) {
		return "", errors.New("session: blob path escapes session")
	}
	return path, nil
}

// Put stores data as an immutable blob.  The returned hash is SHA-256(data).
func (s *ArtifactStore) Put(data []byte) (BlobRef, error) {
	return s.PutContext(context.Background(), data)
}

func (s *ArtifactStore) PutContext(ctx context.Context, data []byte) (BlobRef, error) {
	return s.PutReader(ctx, bytes.NewReader(data))
}

// PutReader is the bounded streaming form used for large tool output and
// attachments.  Context cancellation is checked while reading.
func (s *ArtifactStore) PutReader(ctx context.Context, reader io.Reader) (BlobRef, error) {
	if s == nil {
		return BlobRef{}, errors.New("session: nil artifact store")
	}
	if s.readOnly {
		return BlobRef{}, ErrReadOnly
	}
	if reader == nil {
		return BlobRef{}, errors.New("session: nil artifact reader")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return BlobRef{}, err
	}
	tmp, err := os.CreateTemp(s.dir, ".blob.tmp-")
	if err != nil {
		return BlobRef{}, err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return BlobRef{}, err
	}
	hasher := sha256.New()
	var size int64
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return BlobRef{}, err
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			size += int64(n)
			if s.maxBytes > 0 && size > s.maxBytes {
				return BlobRef{}, fmt.Errorf("%w: blob exceeds %d bytes", ErrTooLarge, s.maxBytes)
			}
			if err := writeAll(tmp, buffer[:n]); err != nil {
				return BlobRef{}, err
			}
			if _, err := hasher.Write(buffer[:n]); err != nil {
				return BlobRef{}, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return BlobRef{}, readErr
		}
		if n == 0 {
			return BlobRef{}, io.ErrNoProgress
		}
	}
	if err := syncFile(tmp, s.syncFile); err != nil {
		return BlobRef{}, err
	}
	if err := tmp.Close(); err != nil {
		return BlobRef{}, err
	}
	sum := hasher.Sum(nil)
	hash := hex.EncodeToString(sum)
	ref := BlobRef{Hash: hash, Size: size}
	path, err := s.path(ref)
	if err != nil {
		return BlobRef{}, err
	}
	if stat, statErr := os.Stat(path); statErr == nil {
		if stat.Size() != size {
			return BlobRef{}, fmt.Errorf("session: immutable blob %s has unexpected size", hash)
		}
		if err := s.Verify(ref); err != nil {
			return BlobRef{}, err
		}
		return ref, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return BlobRef{}, statErr
	}
	// A hard link publishes the already-synced inode without replacing an
	// immutable blob another process won the race to publish.
	if err := os.Link(tmpPath, path); err != nil {
		if stat, statErr := os.Stat(path); statErr == nil && stat.Size() == size {
			if verifyErr := s.Verify(ref); verifyErr != nil {
				return BlobRef{}, verifyErr
			}
			return ref, nil
		}
		return BlobRef{}, err
	}
	removeTemp = false
	_ = os.Remove(tmpPath)
	if err := syncDir(s.dir, s.syncDir); err != nil {
		return BlobRef{}, err
	}
	return ref, nil
}

// Open returns an immutable blob reader after validating its reference.
func (s *ArtifactStore) Open(ref BlobRef) (io.ReadCloser, error) {
	if s == nil {
		return nil, errors.New("session: nil artifact store")
	}
	path, err := s.path(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if stat.Size() != ref.Size {
		_ = f.Close()
		return nil, fmt.Errorf("session: blob %s size changed", ref.Hash)
	}
	if s.maxBytes > 0 && stat.Size() > s.maxBytes {
		_ = f.Close()
		return nil, fmt.Errorf("%w: blob exceeds %d bytes", ErrTooLarge, s.maxBytes)
	}
	// The filename is the content identity.  Verify before exposing bytes so
	// an external same-size edit cannot silently enter a recovered manifest.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != ref.Hash {
		_ = f.Close()
		return nil, fmt.Errorf("session: blob hash mismatch: got %s, want %s", got, ref.Hash)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Read reads a bounded artifact into memory.  maxBytes <= 0 uses the
// ArtifactStore limit.
func (s *ArtifactStore) Read(ref BlobRef, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = s.maxBytes
	}
	reader, err := s.Open(ref)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readBounded(reader, maxBytes)
}

func (s *ArtifactStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Verify recomputes a blob's content hash without mutating it.  It is useful
// to detect an external edit to a supposedly immutable session directory.
func (s *ArtifactStore) Verify(ref BlobRef) error {
	r, err := s.Open(ref)
	if err != nil {
		return err
	}
	return r.Close()
}

func readBounded(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxBlobBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: blob exceeds %d bytes", ErrTooLarge, max)
	}
	return data, nil
}
