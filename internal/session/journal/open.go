package journal

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/session"
	"ccdp/internal/sessionview"
)

type OpenOptions struct {
	SessionID, SessionDir, Source, ParentID, Workspace, Model string
	CreatedAt                                                 time.Time
	NoSessionPersistence, RejectExisting                      bool
	Backend                                                   BackendOptions
}

func validateID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("agent: invalid session id %q", id)
	}
	return nil
}
func Open(o OpenOptions) (*Writer, error) {
	cfg := o
	sessionID, createdAt, source, rejectExisting, parentID := o.SessionID, o.CreatedAt, o.Source, o.RejectExisting, o.ParentID
	backend := []BackendOptions{o.Backend}

	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	if parentID != "" {
		if err := validateID(parentID); err != nil {
			return nil, fmt.Errorf("agent: invalid parent session id: %w", err)
		}
	}

	p := &Writer{memory: cfg.NoSessionPersistence, sessionID: sessionID, closeTransactionID: newSessionLifecycleID(sessionID), transcript: &sessionview.Transcript{}}
	if cfg.NoSessionPersistence {
		p.store = session.NewMemoryStore()
		// Memory mode still has to retain the complete prepared wire body for
		// the lifetime of this Agent. A nil ArtifactStore is not permission to
		// drop request bytes or silently fall back to the session directory.
		p.artifactAccess = NewMemoryArtifacts(session.DefaultMaxBlobBytes)
	} else {
		store, err := session.OpenJSONLStore(cfg.SessionDir, sessionID)
		if err != nil {
			return nil, err
		}
		p.store = store
		p.dir = store.Dir()
		artifacts, err := session.NewArtifactStore(store.Dir())
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("agent: open session artifacts: %w", err)
		}
		p.artifactAccess = artifacts
	}

	if len(backend) > 0 {
		if decorate := backend[0].Store; decorate != nil {
			p.store = decorate(p.store)
		}
		if decorate := backend[0].Artifacts; decorate != nil {
			p.artifactAccess = decorate(p.artifactAccess)
		}
	}

	if p.store.CurrentCursor() != session.Beginning && rejectExisting {
		_ = p.Close()
		return nil, fmt.Errorf("agent: session %q already exists; use Resume", sessionID)
	}
	if p.store.CurrentCursor() == session.Beginning {
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		initialEvents := []session.Event{session.SessionCreated{
			SessionID:     sessionID,
			FormatVersion: session.SchemaVersion,
			Source:        source,
			ParentID:      parentID,
			CreatedAt:     createdAt,
		}}
		if cfg.Workspace != "" || cfg.Model != "" {
			initialEvents = append(initialEvents, session.SettingsChanged{Revision: 1, Settings: session.Settings{
				Workspace: cfg.Workspace,
				Model:     cfg.Model,
			}})
		}
		_, err := p.commitLocked(session.Batch{
			TransactionID: "session-created-" + sessionID,
			Events:        initialEvents,
		})
		if err != nil {
			_ = p.store.Close()
			return nil, fmt.Errorf("agent: persist session creation: %w", err)
		}
	}
	records, readErr := p.store.Read(session.Beginning)
	if readErr != nil {
		_ = p.Close()
		return nil, readErr
	}
	p.transcript = sessionview.FromRecords(records)
	p.requests = RequestIndex{}
	for _, record := range records {
		p.requests.Apply(record.Event, false)
	}
	p.confirmedCursor.Store(p.store.CurrentCursor())
	return p, nil
}
