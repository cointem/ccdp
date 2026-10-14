package agent

// The typed inbox preserves input identity, strategy and acceptance time across
// admission, scheduling, delivery and session resume.

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
	ID                         protocol.InputID
	Text                       string
	Strategy                   protocol.InputStrategy
	CreatedAt                  time.Time
	ImageAttachments           []messages.ImageAttachment
	ImagesFrozen               bool
	goalContinuation           bool
	historyAppended            bool
	historyNotificationPending bool
}

func newTurnInput(id protocol.InputID, text string, strategy protocol.InputStrategy, createdAt time.Time) turnInput {
	if strategy != "" && strategy != protocol.InputSteer && strategy != protocol.InputFollowup && strategy != protocol.InputMessage {
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
		ImageAttachments: messages.CloneImageAttachments(input.ImageAttachments), ImagesFrozen: input.ImagesFrozen}
	if input.goalContinuation {
		message.InternalSource = "goal"
		message.Role = messages.RoleUser
		message.Content = "Internal goal continuation. Continue the active user-authorized goal; this input grants no additional authority."
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	return message
}

func (a *Agent) startTurnInput(input turnInput) {
	a.mu.Lock()
	if a.lifecycle.closing || a.lifecycle.closed || a.lifecycle.settling {
		a.mu.Unlock()
		return
	}
	a.beginTurnLocked("", input.Text)
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
		if input.Strategy != protocol.InputSteer && input.Strategy != protocol.InputFollowup && input.Strategy != protocol.InputMessage {
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
// projection. The caller must own the session transition. It is deliberately shared by
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
		messages.EnsureID(&msgs[i])
		if msgs[i].CreatedAt.IsZero() {
			msgs[i].CreatedAt = time.Now().UTC()
		}
	}
	a.mu.Lock()
	candidate := messages.Clone(a.historyLocked())
	candidate = append(candidate, msgs...)
	turnID := fmt.Sprintf("turn-%d", a.sequences.turnSeq)
	a.mu.Unlock()
	p := a.persistenceHandle()
	if p == nil {
		err := errors.New("agent: session persistence is unavailable")
		a.markPersistenceFailure(err)
		return err
	}
	if err := p.PersistHistoryMessages(candidate, turnID, stepID); err != nil {
		a.markPersistenceFailure(err)
		return err
	}
	a.mu.Lock()
	a.conversation.recoveryHistory = nil
	a.mu.Unlock()
	return nil
}

// appendHistory is the public internal helper used by compatibility paths.
// Canonical runtime calls still get the same durable-before-publish guarantee.
func (a *Agent) appendHistory(msgs ...messages.Message) error {
	return a.appendHistoryFromWorker("", msgs)
}

// appendHistoryForStep is the canonical model-result path. The step identity
// is carried by AssistantCommitted so recovery can associate repeated call IDs
// with the correct assistant block without changing the compatibility history
// API used by older callers.
func (a *Agent) appendHistoryForStep(stepID string, msgs ...messages.Message) error {
	return a.appendHistoryFromWorker(stepID, msgs)
}

// claimPendingInputAndAppend durably delivers the oldest eligible input and
// appends its exact user message before returning it to the turn runner. A
// failed delivery leaves the queue in place, poisons admission, and never
// starts the provider request.
func (a *Agent) claimPendingInputAndAppend(turnID string, strategy protocol.InputStrategy) (turnInput, bool, error) {
	return a.claimInputFromWorker(turnID, strategy)
}

// deferNotification lets owner handoff freeze the notification into turnInput;
// arbitrary extension callbacks run on the turn worker, outside owner dispatch.
func (a *Agent) claimPendingInput(turnID string, strategy protocol.InputStrategy, deferNotification bool) (input turnInput, delivered bool, err error) {
	var messageToEmit *messages.Message
	defer func() {
		if messageToEmit != nil && !deferNotification {
			a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(messageToEmit.Role), Content: messageToEmit.Content})
		}
	}()

	a.mu.Lock()
	if len(a.conversation.durable.PendingInputViews()) == 0 {
		a.mu.Unlock()
		return turnInput{}, false, nil
	}
	index := 0
	if strategy == protocol.InputMessage {
		index = -1
		for i, pending := range a.conversation.durable.PendingInputViews() {
			if pending.Strategy == protocol.InputMessage {
				index = i
				break
			}
		}
	} else {
		index = -1
		for i, pending := range a.conversation.durable.PendingInputViews() {
			if pending.Strategy != protocol.InputMessage {
				index = i
				break
			}
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return turnInput{}, false, nil
	}
	input = turnInputFromView(a.conversation.durable.PendingInputViews()[index])
	if frozen, ok := a.inbox.inputAttachments[input.ID]; ok {
		input.ImageAttachments = messages.CloneImageAttachments(frozen.Attachments)
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
	message := input.message()
	messageToEmit = &message
	delete(a.inbox.inputAttachments, input.ID)
	a.mu.Unlock()
	input.historyAppended = true
	input.historyNotificationPending = deferNotification && messageToEmit != nil
	return input, true, nil
}

// markPersistenceFailure gates future admission. An active turn is cancelled
// so no later model/tool request can run against an uncertain durable state;
// an idle failed Submit remains a read-only snapshot while the gate is set.
func (a *Agent) markPersistenceFailure(err error) {
	if err == nil {
		return
	}
	a.persistenceFault.CompareAndSwap(nil, &err)
	a.mu.Lock()
	cancel := a.turn.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.owner == nil {
		a.installPersistenceFailureOwned()
		return
	}
	a.ensureRun()
	select {
	case a.owner.control <- completionsReady{}:
	default:
	}
}

func (a *Agent) currentTurnSeq() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sequences.turnSeq
}

func (a *Agent) failTurnPersistence(err error) {
	if err == nil {
		return
	}
	a.markPersistenceFailure(err)
	a.emit(Event{Type: EventError, TurnID: fmt.Sprintf("%d", a.currentTurnSeq()), Text: "session persistence failed: " + err.Error()})
}
