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

var fileReadState = struct {
	mu    sync.Mutex
	paths map[string]fileReadEntry
}{paths: map[string]fileReadEntry{}}

// MarkFileRead records the observed state of a path after a successful Read.
func MarkFileRead(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	fileReadState.mu.Lock()
	fileReadState.paths[path] = fileReadEntry{mtime: info.ModTime(), size: info.Size()}
	fileReadState.mu.Unlock()
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
	fileReadState.mu.Unlock()
}

// ClearFileReadState wipes all freshness records (used between tests).
func ClearFileReadState() {
	fileReadState.mu.Lock()
	fileReadState.paths = map[string]fileReadEntry{}
	fileReadState.mu.Unlock()
}
