package tools

import (
	"fmt"
	"sync"

	"ccdp/internal/fsops"
)

// FileState keeps lightweight version observations for the lifetime of a session.
type FileState struct {
	owner  string
	mu     sync.Mutex
	paths  map[string]fsops.Version
	closed bool
}

func NewFileState(owner string) *FileState {
	return &FileState{owner: owner, paths: map[string]fsops.Version{}}
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
}
func (s *FileState) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = map[string]fsops.Version{}
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
	s.paths[path] = v
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
