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

type durableInput struct {
	digest       string
	body         protocol.SubmitInput
	delivered    bool
	cancelled    bool
	cancelReason string
}

// Input identity belongs to the session, including after its consuming run
// finishes or cancels. Both cold admission and child continuation use these facts.
func durableInputs(records []session.Record) map[protocol.InputID]*durableInput {
	inputs := make(map[protocol.InputID]*durableInput)
	for _, record := range records {
		switch event := session.EventPointer(record.Event).(type) {
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
				input.cancelReason = event.Reason
			}
		}
	}
	return inputs
}

func (input *durableInput) inputDigest() (string, error) {
	if input.digest != "" {
		// This is the digest before clipboard locators were appended to Text.
		return input.digest, nil
	}
	return submitInputDigest(input.body)
}

func (input *durableInput) receipt(commandID protocol.CommandID, sessionID protocol.SessionID) protocol.Receipt {
	receipt := protocol.Receipt{CommandID: commandID, SessionID: sessionID, Status: protocol.ReceiptScheduled}
	if input.cancelled {
		reason := input.cancelReason
		if reason == "" {
			reason = "input was cancelled"
		}
		receipt.Status = protocol.ReceiptRejected
		receipt.Error = &protocol.CommandError{Code: protocol.ErrorInvalidState, Message: reason}
	} else if input.delivered {
		receipt.Status = protocol.ReceiptApplied
	}
	return receipt
}

// restoreInputDedup rebuilds input identity from durable queue facts. Cancelled
// identities remain terminal instead of being admitted under the same ID again.
func (a *Agent) restoreInputDedup() error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, input := range durableInputs(records) {
		if id == "" {
			continue
		}
		a.inbox.seenInputs[id] = input.receipt(protocol.CommandID(id), protocol.SessionID(a.conversation.sessionID))
		digest, digestErr := input.inputDigest()
		if digestErr != nil {
			return fmt.Errorf("encode resumed input identity: %w", digestErr)
		}
		a.inbox.seenInputBody[id] = digest
	}
	return nil
}
