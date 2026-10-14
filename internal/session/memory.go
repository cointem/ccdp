package session

import (
	"sync"
)

// MemoryStore is the no-persistence implementation used by tests and by an
// explicitly selected --no-session-persistence mode.  It deliberately uses
// the same commit preparation and event codec as JSONLStore.
type MemoryStore struct {
	mu     sync.Mutex
	state  logState
	closed bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: newLogState()}
}

func (s *MemoryStore) Commit(expected Cursor, batch Batch) (CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CommitResult{}, ErrClosed
	}
	prepared, err := prepareCommit(&s.state, expected, batch, defaultJSONLOptions())
	if err != nil {
		return CommitResult{}, err
	}
	applyPrepared(&s.state, prepared)
	return prepared.Result, nil
}

func (s *MemoryStore) Read(after Cursor) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var records []Record
	for _, record := range s.state.Records {
		if record.Seq > after {
			records = append(records, record)
		}
	}
	return cloneRecords(records)
}

func (s *MemoryStore) CurrentCursor() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Cursor
}

// PersistenceEnabled is useful for status bars; memory mode intentionally
// does not claim that a receipt survived a process exit.
func (s *MemoryStore) PersistenceEnabled() bool { return false }

func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
