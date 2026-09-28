package tools

import (
	"fmt"
	"sync"
	"time"

	"ccdp/internal/fsops"
)

// FileState stores bounded successful version observations, independent of display limits.
type FileReadRecord struct {
	Path  string
	Mtime time.Time
}
type FileState struct {
	owner  string
	mu     sync.Mutex
	paths  map[string]fsops.Version
	order  []string
	closed bool
}

func NewFileState(owner string) *FileState {
	return &FileState{owner: owner, paths: map[string]fsops.Version{}}
}
func (s *FileState) Owner() string {
	if s == nil {
		return ""
	}
	return s.owner
}
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
func (s *FileState) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.paths = map[string]fsops.Version{}
	s.order = nil
}
func (s *FileState) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = map[string]fsops.Version{}
	s.order = nil
}
func (s *FileState) ForgetFile(path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(path)
}
func (s *FileState) forget(path string) {
	delete(s.paths, path)
	for i, p := range s.order {
		if p == path {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}
func (s *FileState) MarkVersion(path string, v fsops.Version) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.forget(path)
	s.paths[path] = v
	s.order = append(s.order, path)
	for len(s.order) > 32 {
		s.forget(s.order[0])
	}
}
func (s *FileState) ObservedVersion(path string) (fsops.Version, error) {
	if err := s.CheckOpen(); err != nil {
		return fsops.Version{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.paths[path]
	if !ok {
		return v, fmt.Errorf("file safety: %s has not been read yet; Read before replacing it", path)
	}
	return v, nil
}
func (s *FileState) RecentReads(n int) []FileReadRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FileReadRecord
	for i := len(s.order) - 1; i >= 0 && len(out) < n; i-- {
		p := s.order[i]
		out = append(out, FileReadRecord{p, s.paths[p].Mtime})
	}
	return out
}
