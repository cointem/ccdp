package agent

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// SessionSnapshotProjection replays committed facts into the session view.
func SessionSnapshotProjection(store session.ReadOnlyStore, id string) (*SessionSnapshot, error) {
	if store == nil {
		return nil, errors.New("agent: nil session store")
	}
	records, err := session.ReadAll(store)
	if err != nil {
		return nil, err
	}
	return snapshotFromRecords(records, id)
}

func (p *sessionPersistence) snapshotFromOwnedStore(id string) (*SessionSnapshot, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	records, err := session.ReadAll(p)
	if err != nil {
		return nil, err
	}
	return snapshotFromRecords(records, id)
}

// populateMemoryResume seeds the per-process MemoryStore used by
// --no-session-persistence Resume. There is no disk log to replay in that
// mode, so the caller's explicit snapshot must be converted into the same
// typed facts that a JSONL replay would expose before the owned-store replay
// boundary runs. It is intentionally a no-op for JSONL stores.
func (p *sessionPersistence) populateMemoryResume(snapshot *SessionSnapshot) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if snapshot == nil {
		return errors.New("agent: memory resume snapshot is required")
	}
	needed, err := p.Writer.NeedsMemoryResume(snapshot.ID)
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	prepared := *snapshot
	prepared.History = messages.Clone(snapshot.History)
	prepared.PendingAttachments = make(map[string][]messages.ImageAttachment, len(snapshot.PendingAttachments))
	for _, message := range prepared.History {
		if _, err := p.Writer.EncodeMessage(message); err != nil {
			return err
		}
	}
	for id, images := range snapshot.PendingAttachments {
		images = messages.CloneImageAttachments(images)
		if _, err := p.Writer.EncodeMessage(messages.Message{Role: messages.RoleUser, ImageAttachments: images, ImagesFrozen: true}); err != nil {
			return err
		}
		prepared.PendingAttachments[id] = images
	}
	events, err := memoryResumeEvents(prepared)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return errors.New("agent: memory resume has no events")
	}
	baseID := messages.StableID("memory-resume", struct {
		ID            string
		CreatedAt     time.Time
		UpdatedAt     time.Time
		History       []messages.Message
		Usage         Usage
		PendingInputs []protocol.InputView
		ParentID      string
		BranchPoint   int
		BranchSummary string
	}{snapshot.ID, snapshot.CreatedAt, snapshot.UpdatedAt, snapshot.History, snapshot.Usage, snapshot.PendingInputs, snapshot.ParentID, snapshot.BranchPoint, snapshot.BranchSummary})
	for offset, chunk := 0, 0; offset < len(events); chunk++ {
		end := offset + 128
		if end > len(events) {
			end = len(events)
		}
		if _, err := p.Commit(session.Batch{
			TransactionID: fmt.Sprintf("%s-%04d", baseID, chunk),
			Events:        events[offset:end],
		}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
