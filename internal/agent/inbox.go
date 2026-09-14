package agent

// The typed inbox is the runtime's admission boundary.  The old pendingMsgs
// slice remains only as a compatibility projection for legacy callers; all
// new scheduling, delivery, and resume paths use pendingInputs and preserve
// the input identity, strategy, and acceptance timestamp.

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type turnInput struct {
	ID               protocol.InputID
	Text             string
	Strategy         protocol.InputStrategy
	CreatedAt        time.Time
	ImageAttachments []messages.ImageAttachment
	ImagesFrozen     bool
	historyAppended  bool
}

func newTurnInput(id protocol.InputID, text string, strategy protocol.InputStrategy, createdAt time.Time) turnInput {
	if strategy != "" && strategy != protocol.InputSteer && strategy != protocol.InputFollowup {
		strategy = protocol.InputFollowup
	}
	return turnInput{ID: id, Text: text, Strategy: strategy, CreatedAt: createdAt}
}

func turnInputFromView(view protocol.InputView) turnInput {
	return newTurnInput(view.ID, view.Text, view.Strategy, view.CreatedAt)
}

func (input turnInput) view() protocol.InputView {
	return protocol.InputView{ID: input.ID, Text: input.Text, Strategy: input.Strategy, State: "queued", CreatedAt: input.CreatedAt}
}

func (input turnInput) message() messages.Message {
	message := messages.Message{ID: inputMessageID(string(input.ID)), Role: messages.RoleUser, Content: input.Text, CreatedAt: input.CreatedAt,
		ImageAttachments: cloneImageAttachments(input.ImageAttachments), ImagesFrozen: input.ImagesFrozen}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	return message
}

func cloneImageAttachments(src []messages.ImageAttachment) []messages.ImageAttachment {
	if src == nil {
		return nil
	}
	dst := make([]messages.ImageAttachment, len(src))
	for i, attachment := range src {
		dst[i] = attachment
		dst[i].Data = append([]byte(nil), attachment.Data...)
	}
	return dst
}

// ensureTypedPendingLocked upgrades the compatibility text queue at the one
// boundary where it can still be observed.  The typed queue is canonical
// after this function returns; pendingMsgs is regenerated from it so the two
// projections cannot drift during Save or a turn transition.
func (a *Agent) ensureTypedPendingLocked() {
	if len(a.pendingInputs) == 0 && len(a.pendingMsgs) > 0 {
		inputs := make([]protocol.InputView, 0, len(a.pendingMsgs))
		for i, text := range a.pendingMsgs {
			if text == "" {
				continue
			}
			id := protocol.InputID(stableID("legacy-pending", struct {
				SessionID string
				Index     int
				Text      string
			}{a.sessionID, i, text}))
			inputs = append(inputs, protocol.InputView{ID: id, Text: text, Strategy: protocol.InputFollowup, State: "queued", CreatedAt: time.Now().UTC()})
		}
		a.pendingInputs = inputs
	}
	a.pendingMsgs = pendingTexts(a.pendingInputs)
}

func (a *Agent) syncLegacyPendingLocked() {
	a.pendingMsgs = pendingTexts(a.pendingInputs)
}

func (a *Agent) startTurnInput(input turnInput) {
	a.mu.Lock()
	if a.closing || a.closed || a.settling {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.turnSeq++
	a.turnWG.Add(1)
	a.phase = protocol.PhasePreparing
	a.mu.Unlock()
	a.publishState()
	go a.runTurn(input)
}

// hydrateInput fills metadata omitted by a compatibility snapshot from the
// durable InputQueued fact. New projections already include CreatedAt, but a
// resumed legacy snapshot must not silently replace that timestamp with the
// time at which the input happens to be delivered.
func (a *Agent) hydrateInput(input *turnInput) error {
	if input == nil || !input.CreatedAt.IsZero() {
		return nil
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	for _, record := range records {
		queued, ok := record.Event.(*session.InputQueued)
		if !ok || queued.InputID != string(input.ID) {
			continue
		}
		if input.Text == "" {
			input.Text = queued.Text
		}
		if input.Strategy != protocol.InputSteer && input.Strategy != protocol.InputFollowup {
			input.Strategy = protocol.InputStrategy(queued.Strategy)
		}
		input.CreatedAt = queued.CreatedAt
		break
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	return nil
}

// appendHistoryLocked commits a candidate history before swapping the live
// projection. The caller must hold persistMu. It is deliberately shared by
// input delivery, model results, and Save so a failed store never becomes a
// confirmed in-memory message.
func (a *Agent) appendHistoryLocked(msgs ...messages.Message) error {
	return a.appendHistoryLockedForStep("", msgs...)
}

func (a *Agent) appendHistoryLockedForStep(stepID string, msgs ...messages.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	for i := range msgs {
		ensureMessageID(&msgs[i])
		if msgs[i].CreatedAt.IsZero() {
			msgs[i].CreatedAt = time.Now().UTC()
		}
	}
	a.mu.Lock()
	candidate := cloneMessages(a.history)
	candidate = append(candidate, msgs...)
	turnID := fmt.Sprintf("turn-%d", a.turnSeq)
	a.mu.Unlock()
	p := a.persistenceHandle()
	if p == nil {
		err := errors.New("agent: session persistence is unavailable")
		a.markPersistenceFailure(err)
		return err
	}
	if err := p.persistHistoryMessages(candidate, turnID, stepID); err != nil {
		a.markPersistenceFailure(err)
		return err
	}
	a.mu.Lock()
	a.history = candidate
	a.mu.Unlock()
	return nil
}

// appendHistory is the public internal helper used by compatibility paths.
// Canonical runtime calls still get the same durable-before-publish guarantee.
func (a *Agent) appendHistory(msgs ...messages.Message) error {
	a.persistMu.Lock()
	err := a.appendHistoryLocked(msgs...)
	a.persistMu.Unlock()
	if err != nil {
		return err
	}
	// Event-bus subscribers are allowed to call Save. Emit only after the
	// durable projection mutex is released; synchronous delivery under that
	// lock would deadlock such a subscriber.
	for _, message := range msgs {
		a.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
	}
	return nil
}

// appendHistoryForStep is the canonical model-result path. The step identity
// is carried by AssistantCommitted so recovery can associate repeated call IDs
// with the correct assistant block without changing the compatibility history
// API used by older callers.
func (a *Agent) appendHistoryForStep(stepID string, msgs ...messages.Message) error {
	a.persistMu.Lock()
	err := a.appendHistoryLockedForStep(stepID, msgs...)
	a.persistMu.Unlock()
	if err != nil {
		return err
	}
	for _, message := range msgs {
		a.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
	}
	return nil
}

// claimPendingInputAndAppend durably delivers the oldest eligible input and
// appends its exact user message before returning it to the turn runner. A
// failed delivery leaves the queue in place, poisons admission, and never
// starts the provider request.
func (a *Agent) claimPendingInputAndAppend(turnID string, strategy protocol.InputStrategy) (input turnInput, delivered bool, err error) {
	a.persistMu.Lock()
	var messageToEmit *messages.Message
	defer func() {
		a.persistMu.Unlock()
		if messageToEmit != nil {
			a.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(messageToEmit.Role), Content: messageToEmit.Content})
		}
	}()

	a.mu.Lock()
	a.ensureTypedPendingLocked()
	if len(a.pendingInputs) == 0 {
		a.mu.Unlock()
		return turnInput{}, false, nil
	}
	input = turnInputFromView(a.pendingInputs[0])
	if frozen, ok := a.inputAttachments[input.ID]; ok {
		input.ImageAttachments = cloneImageAttachments(frozen.Attachments)
		input.ImagesFrozen = frozen.Frozen
	}
	a.mu.Unlock()
	if strategy != "" && input.Strategy != strategy {
		return turnInput{}, false, nil
	}
	if input.ID == "" {
		return turnInput{}, false, errors.New("agent: queued input has no identity")
	}
	if err := a.hydrateInput(&input); err != nil {
		a.markPersistenceFailure(err)
		return turnInput{}, false, err
	}
	if err := a.persistInputDelivered(string(input.ID), turnID); err != nil {
		a.markPersistenceFailure(err)
		return turnInput{}, false, err
	}

	a.mu.Lock()
	a.ensureTypedPendingLocked()
	if len(a.pendingInputs) == 0 || a.pendingInputs[0].ID != input.ID || a.pendingInputs[0].Text != input.Text {
		a.mu.Unlock()
		err := errors.New("agent: queued input changed during durable delivery")
		a.markPersistenceFailure(err)
		return turnInput{}, false, err
	}
	a.pendingInputs = a.pendingInputs[1:]
	a.syncLegacyPendingLocked()
	message := input.message()
	alreadyPresent := false
	for _, existing := range a.history {
		if existing.ID == message.ID {
			alreadyPresent = true
			break
		}
	}
	if !alreadyPresent {
		a.history = append(a.history, message)
		messageToEmit = &message
	}
	delete(a.inputAttachments, input.ID)
	a.mu.Unlock()
	input.historyAppended = true
	return input, true, nil
}

// markPersistenceFailure gates future admission. An active turn is cancelled
// so no later model/tool request can run against an uncertain durable state;
// an idle failed Submit remains a read-only snapshot while the gate is set.
func (a *Agent) markPersistenceFailure(err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	if a.persistenceErr == nil {
		a.persistenceErr = err
	}
	cancel := a.turnCancel
	if a.busy {
		a.stop = true
		a.interruptFlag = true
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Agent) currentTurnSeq() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnSeq
}

func (a *Agent) failTurnPersistence(err error) {
	if err == nil {
		return
	}
	a.markPersistenceFailure(err)
	a.emit(Event{Type: EventError, TurnID: fmt.Sprintf("%d", a.currentTurnSeq()), Text: "session persistence failed: " + err.Error()})
}
