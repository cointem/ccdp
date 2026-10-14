package journal

import (
	"errors"
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/sessionview"
)

// NeedsMemoryResume is used only while the constructor owns the unpublished
// writer. A durable memory-resume marker makes retried construction idempotent.
func (p *Writer) NeedsMemoryResume(id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return false, session.ErrClosed
	}
	if !p.memory {
		return false, nil
	}
	if p.failed != nil {
		return false, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if id == "" {
		return false, errors.New("agent: memory resume snapshot has no session id")
	}
	if err := validateID(id); err != nil {
		return false, err
	}
	if p.sessionID != "" && p.sessionID != id {
		return false, fmt.Errorf("agent: memory resume session %q does not match persistence %q", id, p.sessionID)
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return false, err
	}
	for _, r := range records {
		if e, ok := r.Event.(*session.SessionImported); ok && e.Source == "memory-resume" {
			return false, nil
		}
	}
	return true, nil
}

// EncodeMessage freezes blob identities before a construction-time import.
func (p *Writer) EncodeMessage(m messages.Message) (session.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.Message{}, session.ErrClosed
	}
	return p.messageToSessionLocked(m)
}

// In-process continuation preserves the same log and artifacts even when
// persistence is disabled. It never creates a hidden on-disk session.
func (p *Writer) RestoreMemoryRun(records []session.Record, blobs ArtifactAccess) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	store := session.NewMemoryStore()
	for i := 0; i < len(records); {
		batch := session.Batch{TransactionID: records[i].TransactionID}
		for i < len(records) && records[i].TransactionID == batch.TransactionID {
			batch.Events = append(batch.Events, records[i].Event)
			i++
		}
		if _, err := store.Commit(store.CurrentCursor(), batch); err != nil {
			_ = store.Close()
			return err
		}
	}
	_ = p.store.Close()
	p.store = store
	p.artifactAccess = blobs
	p.projection = nil
	p.projectionCursor = 0
	p.transcript = sessionview.FromRecords(records)
	p.requests = RequestIndex{}
	for _, record := range records {
		p.requests.Apply(record.Event, false)
	}
	p.confirmedCursor.Store(store.CurrentCursor())
	_, err := p.projectionLocked()
	return err
}
