package agent

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"ccdp/internal/changes"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/session/journal"
)

// Memory sessions retain their own records and child runs, shared by handles
// opened from the same catalog. Closing one Agent does not invalidate another
// handle's source. There is no process-global registry or hidden disk persistence.
type memorySessionCatalog struct {
	mu       sync.Mutex
	sessions map[string]*memorySession
}

type memorySession struct {
	snapshot *archive.Snapshot
	coding   *changes.Store
	children map[protocol.RunID]memoryRunRecords
}

type memoryRunRecords struct {
	// Both records and blob contents are immutable. No closed child runtime
	// is retained; continuation constructs a fresh owner from these facts.
	records []session.Record
	blobs   journal.ArtifactAccess
}

func (a *Agent) memorySessionCatalog() *memorySessionCatalog {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memorySessions == nil {
		a.memorySessions = &memorySessionCatalog{sessions: make(map[string]*memorySession)}
	}
	return a.memorySessions
}

func (a *Agent) retainMemorySession(c *memorySessionCatalog) error {
	snapshot, err := readMemorySession(a.persistenceHandle())
	if err != nil {
		return err
	}
	// Coding facts refer to a separate store using plain hashes, not BlobRefs.
	// Capture it after the log so every recorded reference has its object.
	savedSession := &memorySession{snapshot: snapshot, coding: a.codingState().store.Clone(), children: make(map[protocol.RunID]memoryRunRecords)}
	for id, saved := range a.services.supervisor.memoryChildren {
		savedSession.children[id] = saved
	}
	for _, run := range a.services.supervisor.directory.Runs() {
		run.mu.Lock()
		if run.fact.Child.RootSessionID != protocol.SessionID(snapshot.ID) &&
			run.fact.Child.ParentSessionID != protocol.SessionID(snapshot.ID) {
			run.mu.Unlock()
			continue
		}
		id, child := run.fact.Child.Run.ID, run.agent
		saved := memoryRunRecords{records: append([]session.Record(nil), run.memory...), blobs: run.memoryBlobs}
		run.mu.Unlock()
		if saved.records == nil && child != nil {
			records, artifacts, err := child.ReadRunRecords(context.Background())
			if err != nil {
				return err
			}
			saved.records = records
			saved.blobs, err = copyMemoryArtifacts(records, artifacts)
			if err != nil {
				return err
			}
		}
		if saved.records != nil {
			savedSession.children[id] = saved
		}
	}
	c.mu.Lock()
	c.sessions[snapshot.ID] = savedSession
	c.mu.Unlock()
	return nil
}

func retainMemoryPersistence(c *memorySessionCatalog, p *sessionPersistence) error {
	snapshot, err := readMemorySession(p)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sessions[snapshot.ID] = &memorySession{snapshot: snapshot}
	c.mu.Unlock()
	return nil
}

func readMemorySession(p *sessionPersistence) (*archive.Snapshot, error) {
	records, artifacts, err := p.ReadSnapshot()
	if err != nil {
		return nil, err
	}
	snapshot, err := archive.FromRecords(records, p.Identity())
	if err != nil {
		return nil, err
	}
	snapshot.Artifacts = artifacts
	return snapshot, nil
}

func copyMemoryArtifacts(records []session.Record, artifacts journal.ArtifactReader) (journal.ArtifactAccess, error) {
	blobs := journal.NewMemoryArtifacts(session.DefaultMaxBlobBytes)
	seen := make(map[string]bool)
	refType := reflect.TypeOf(session.BlobRef{})
	var copyRefs func(reflect.Value) error
	copyRefs = func(v reflect.Value) error {
		if !v.IsValid() {
			return nil
		}
		if v.Type() == refType {
			ref := v.Interface().(session.BlobRef)
			if ref.Hash == "" || seen[ref.Hash] {
				return nil
			}
			if artifacts == nil {
				return fmt.Errorf("session has no artifact source")
			}
			data, err := artifacts.Read(ref, session.DefaultMaxBlobBytes)
			if err != nil {
				return err
			}
			if _, err = blobs.PutContext(context.Background(), data); err != nil {
				return err
			}
			seen[ref.Hash] = true
			return nil
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				return copyRefs(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					if err := copyRefs(v.Field(i)); err != nil {
						return err
					}
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return nil
			}
			for i := 0; i < v.Len(); i++ {
				if err := copyRefs(v.Index(i)); err != nil {
					return err
				}
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				if err := copyRefs(iter.Value()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, record := range records {
		if err := copyRefs(reflect.ValueOf(record.Event)); err != nil {
			return nil, err
		}
	}
	return blobs, nil
}
