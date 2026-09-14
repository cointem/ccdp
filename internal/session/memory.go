package session

import (
	"errors"
	"sync"
	"time"
)

// MemoryStore is the no-persistence implementation used by tests and by an
// explicitly selected --no-session-persistence mode.  It deliberately uses
// the same commit preparation and event codec as JSONLStore.
type MemoryStore struct {
	mu       sync.Mutex
	state    logState
	snapshot Snapshot
	hasSnap  bool
	closed   bool
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

func (s *MemoryStore) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, ErrClosed
	}
	if !s.hasSnap {
		return Snapshot{}, ErrSnapshotNotFound
	}
	snapshot := s.snapshot
	snapshot.Data = copyBytes(snapshot.Data)
	return snapshot, nil
}

func (s *MemoryStore) SaveSnapshot(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if snapshot.SchemaVersion == 0 {
		snapshot.SchemaVersion = SchemaVersion
	}
	if snapshot.SchemaVersion > SchemaVersion {
		return ErrFutureVersion
	}
	if snapshot.SchemaVersion != SchemaVersion {
		return ErrSnapshotInvalid
	}
	if snapshot.LastSeq > s.state.Cursor {
		return errors.Join(ErrSnapshotInvalid, &CursorConflictError{Expected: snapshot.LastSeq, Actual: s.state.Cursor})
	}
	if snapshot.SessionID == "" {
		snapshot.SessionID = "memory"
	}
	snapshot.Data = copyBytes(snapshot.Data)
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now().UTC()
	}
	s.snapshot = snapshot
	s.hasSnap = true
	return nil
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
