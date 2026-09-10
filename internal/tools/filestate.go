package tools

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// File freshness tracking (Claude Code's readFileState idea): Read records the
// file's mtime at read time; Edit/Write verify it before mutating. A file that
// changed on disk after the last Read (user edit, linter, formatter) is
// rejected so the agent never overwrites unseen changes. Edit/Write also
// require a prior Read of the same path, mirroring Claude's "read before edit"
// gate.

type fileReadEntry struct {
	mtime time.Time
	size  int64
}

// FileReadRecord is the observable state of one tracked path, for consumers
// that need recency (e.g. the agent's post-compaction file restore).
type FileReadRecord struct {
	Path  string
	Mtime time.Time
}

var fileReadState = struct {
	mu    sync.Mutex
	paths map[string]fileReadEntry
	order []string // read order, oldest first (most recently read last)
}{paths: map[string]fileReadEntry{}}

// MarkFileRead records the observed state of a path after a successful Read.
func MarkFileRead(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	fileReadState.mu.Lock()
	fileReadState.paths[path] = fileReadEntry{mtime: info.ModTime(), size: info.Size()}
	// Move (or append) the path to the end of the recency order.
	for i, p := range fileReadState.order {
		if p == path {
			fileReadState.order = append(fileReadState.order[:i], fileReadState.order[i+1:]...)
			break
		}
	}
	fileReadState.order = append(fileReadState.order, path)
	fileReadState.mu.Unlock()
}

// RecentReads returns up to n most recently read paths, newest first. It backs
// the agent's post-compaction restore (Claude Code re-attaches recently read
// files after compacting so the model keeps its working set).
func RecentReads(n int) []FileReadRecord {
	if n <= 0 {
		return nil
	}
	fileReadState.mu.Lock()
	defer fileReadState.mu.Unlock()
	var out []FileReadRecord
	for i := len(fileReadState.order) - 1; i >= 0 && len(out) < n; i-- {
		p := fileReadState.order[i]
		if e, ok := fileReadState.paths[p]; ok {
			out = append(out, FileReadRecord{Path: p, Mtime: e.mtime})
		}
	}
	return out
}

// CheckFileFresh validates that path may be mutated now. It returns an error
// when the file was never read this session, or when it changed on disk since
// the last Read. Use ForgetFile when the mutation itself is accepted (after a
// successful Edit/Write the caller re-marks via MarkFileRead anyway).
func CheckFileFresh(path string) error {
	fileReadState.mu.Lock()
	entry, ok := fileReadState.paths[path]
	fileReadState.mu.Unlock()
	if !ok {
		return fmt.Errorf("file safety: %s has not been read yet — Read it before editing so no unseen changes are overwritten", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("file safety: %s no longer exists on disk", path)
	}
	if !info.ModTime().Equal(entry.mtime) || info.Size() != entry.size {
		return fmt.Errorf("file safety: %s has been modified since it was last read (by the user or another process) — Read it again to pick up the current content before editing", path)
	}
	return nil
}

// ForgetFile drops the freshness record for a path (e.g. after the agent
// itself wrote it, so the next Edit must Read again only if content changed).
func ForgetFile(path string) {
	fileReadState.mu.Lock()
	delete(fileReadState.paths, path)
	for i, p := range fileReadState.order {
		if p == path {
			fileReadState.order = append(fileReadState.order[:i], fileReadState.order[i+1:]...)
			break
		}
	}
	fileReadState.mu.Unlock()
}

// ClearFileReadState wipes all freshness records (used between tests).
func ClearFileReadState() {
	fileReadState.mu.Lock()
	fileReadState.paths = map[string]fileReadEntry{}
	fileReadState.order = nil
	fileReadState.mu.Unlock()
}
