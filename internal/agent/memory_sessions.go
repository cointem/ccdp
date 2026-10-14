package agent

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"ccdp/internal/config"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

// Memory forks are immutable saved records, shared by handles opened from the
// same session. Closing one Agent does not invalidate another handle's source.
// There is no process-global registry or hidden disk persistence.
type memorySessionCatalog struct {
	mu       sync.Mutex
	sessions map[string]*SessionSnapshot
}

func (a *Agent) memorySessionCatalog() *memorySessionCatalog {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memorySessions == nil {
		a.memorySessions = &memorySessionCatalog{sessions: make(map[string]*SessionSnapshot)}
	}
	return a.memorySessions
}

func (a *Agent) retainMemorySession(c *memorySessionCatalog) error {
	return retainMemoryPersistence(c, a.persistenceHandle())
}

func retainMemoryPersistence(c *memorySessionCatalog, p *sessionPersistence) error {
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	snapshot, err := snapshotFromRecords(records, p.Identity())
	if err != nil {
		return err
	}
	snapshot.resumeRecords = records
	snapshot.resumeArtifacts, err = p.RequestArtifacts()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sessions[snapshot.ID] = snapshot
	c.mu.Unlock()
	return nil
}

// Import complete typed facts and their blobs. Rebuilding facts from a display
// snapshot would lose input identities, frozen attachments and request usage.
func memoryResumeOptions(cfg *config.Config, snapshot *SessionSnapshot) (Options, error) {
	if !cfg.NoSessionPersistence || len(snapshot.resumeRecords) == 0 {
		return Options{}, nil
	}
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
			if snapshot.resumeArtifacts == nil {
				return fmt.Errorf("session %s has no artifact source", snapshot.ID)
			}
			data, err := snapshot.resumeArtifacts.Read(ref, session.DefaultMaxBlobBytes)
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
	for _, record := range snapshot.resumeRecords {
		if err := copyRefs(reflect.ValueOf(record.Event)); err != nil {
			return Options{}, fmt.Errorf("import session artifacts: %w", err)
		}
	}
	return Options{memoryResume: snapshot.resumeRecords, memoryBlobs: blobs}, nil
}
