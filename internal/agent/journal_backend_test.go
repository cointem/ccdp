package agent

import (
	"context"
	"sync"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

// Fault backends are injected before opening the writer. Tests can fail an I/O
// operation without accessing or replacing the writer's private live backend.
type journalTestBackend struct {
	mu    sync.Mutex
	store session.Store
	blobs journal.ArtifactAccess
}

var journalTestBackends sync.Map

func (b *journalTestBackend) options() journal.BackendOptions {
	return journal.BackendOptions{
		Store:     func(s session.Store) session.Store { b.store = s; return journalTestStore{b} },
		Artifacts: func(a journal.ArtifactAccess) journal.ArtifactAccess { b.blobs = a; return journalTestArtifacts{b} },
	}
}
func journalBackend(t testing.TB, p *sessionPersistence) *journalTestBackend {
	t.Helper()
	v, ok := journalTestBackends.Load(p)
	if !ok {
		t.Fatal("test fixture did not inject a journal backend")
	}
	return v.(*journalTestBackend)
}
func registerJournalBackend(t testing.TB, p *sessionPersistence, b *journalTestBackend) {
	journalTestBackends.Store(p, b)
	t.Cleanup(func() { journalTestBackends.Delete(p) })
}
func testNew(t testing.TB, cfg *config.Config) (*Agent, error) {
	t.Helper()
	backend := &journalTestBackend{}
	a, err := newAgentWithOptions(cfg, nil, Options{journalBackend: backend.options()})
	if err == nil {
		registerJournalBackend(t, a.persistenceHandle(), backend)
	}
	return a, err
}
func testNewWithOptions(t testing.TB, cfg *config.Config, opts Options) (*Agent, error) {
	t.Helper()
	b := &journalTestBackend{}
	opts.journalBackend = b.options()
	a, err := NewWithOptions(cfg, opts)
	if err == nil {
		registerJournalBackend(t, a.persistenceHandle(), b)
	}
	return a, err
}

type journalTestStore struct{ backend *journalTestBackend }

func (s journalTestStore) current() session.Store {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	return s.backend.store
}
func (s journalTestStore) Commit(c session.Cursor, b session.Batch) (session.CommitResult, error) {
	return s.current().Commit(c, b)
}
func (s journalTestStore) Read(c session.Cursor) ([]session.Record, error) {
	return s.current().Read(c)
}
func (s journalTestStore) CurrentCursor() session.Cursor { return s.current().CurrentCursor() }
func (s journalTestStore) Close() error                  { return s.current().Close() }

type journalTestArtifacts struct{ backend *journalTestBackend }

func (s journalTestArtifacts) current() journal.ArtifactAccess {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	return s.backend.blobs
}
func (s journalTestArtifacts) Read(r session.BlobRef, n int64) ([]byte, error) {
	return s.current().Read(r, n)
}
func (s journalTestArtifacts) PutContext(ctx context.Context, b []byte) (session.BlobRef, error) {
	return s.current().PutContext(ctx, b)
}
