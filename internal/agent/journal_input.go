package agent

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// persistInput queues a durable inbox fact.  Runtime admission must call it
// before publishing queued inputs or starting a turn.
func (a *Agent) persistInput(inputID, text string, strategy string, turnID string, createdAt time.Time, attachments ...messages.ImageAttachment) error {
	return a.persistInputWithDigest(inputID, text, strategy, turnID, createdAt, "", attachments...)
}

func (a *Agent) persistInputWithDigest(inputID, text string, strategy string, turnID string, createdAt time.Time, digest string, attachments ...messages.ImageAttachment) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.PersistInputQueuedWithDigest(inputID, text, strategy, turnID, createdAt, digest, attachments...)
}

func (a *Agent) persistInputDelivered(inputID, turnID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.PersistInputDelivered(inputID, turnID)
}

func (a *Agent) persistHistory(history []messages.Message, turnID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.PersistHistoryMessages(history, turnID)
}

// restoreInputDedup rebuilds command-level input identity from durable queue
// facts. A restarted runtime must not execute a previously delivered input a
// second time merely because its in-memory seenInputs map was empty.
func (a *Agent) restoreInputDedup() error {
	if a == nil {
		return errors.New("agent: nil agent")
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	type durableInput struct {
		digest    string
		body      protocol.SubmitInput
		delivered bool
		cancelled bool
	}
	inputs := make(map[protocol.InputID]*durableInput)
	for _, record := range records {
		switch event := record.Event.(type) {
		case *session.InputQueued:
			strategy := protocol.InputStrategy(event.Strategy)
			if strategy != protocol.InputSteer && strategy != protocol.InputFollowup && strategy != protocol.InputMessage {
				strategy = protocol.InputFollowup
			}
			id := protocol.InputID(event.InputID)
			inputs[id] = &durableInput{body: protocol.SubmitInput{ID: id, Text: event.Text, Strategy: strategy}, digest: event.InputDigest}
		case *session.InputDelivered:
			if input := inputs[protocol.InputID(event.InputID)]; input != nil {
				input.delivered = true
			}
		case *session.InputCancelled:
			if input := inputs[protocol.InputID(event.InputID)]; input != nil {
				input.cancelled = true
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, input := range inputs {
		if id == "" || input.cancelled {
			continue
		}
		status := protocol.ReceiptScheduled
		if input.delivered {
			status = protocol.ReceiptApplied
		}
		a.inbox.seenInputs[id] = protocol.Receipt{CommandID: protocol.CommandID(id), SessionID: protocol.SessionID(a.conversation.sessionID), Status: status, OperationID: protocol.OperationID("resume-input-" + string(id))}
		digest, digestErr := submitInputDigest(input.body)
		if digestErr != nil {
			return fmt.Errorf("encode resumed input identity: %w", digestErr)
		}
		a.inbox.seenInputBody[id] = digest
		if input.digest != "" {
			a.inbox.seenInputBody[id] = input.digest
		}
	}
	return nil
}
