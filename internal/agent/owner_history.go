package agent

import (
	"ccdp/internal/events"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type historyAppendRequested struct {
	stepID   string
	messages []messages.Message
	result   chan error
}

func (historyAppendRequested) ownerControlMessage() {}

type inputClaimRequested struct {
	turnID   string
	strategy protocol.InputStrategy
	result   chan inputClaimResult
}

func (inputClaimRequested) ownerControlMessage() {}

type inputClaimResult struct {
	input     turnInput
	delivered bool
	err       error
}

func (a *Agent) commitHistoryAppend(request historyAppendRequested) error {
	return a.appendHistoryLockedForStep(request.stepID, request.messages...)
}

func (a *Agent) appendHistoryFromWorker(stepID string, msgs []messages.Message) error {
	request := historyAppendRequested{stepID: stepID, messages: messages.Clone(msgs), result: make(chan error, 1)}
	var err error
	if a.owner == nil {
		err = a.commitHistoryAppend(request)
	} else {
		a.ensureRun()
		select {
		case a.owner.control <- request:
		case <-a.owner.done:
			return session.ErrClosed
		}
		select {
		case err = <-request.result:
		case <-a.owner.done:
			return session.ErrClosed
		}
	}
	if err != nil {
		return err
	}
	// Extension callbacks may Submit or Save, so their caller stays outside the
	// state loop and all journal critical sections.
	for _, message := range msgs {
		a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
	}
	return nil
}

func (a *Agent) claimInputFromWorker(turnID string, strategy protocol.InputStrategy) (turnInput, bool, error) {
	if a.owner == nil {
		return a.claimPendingInput(turnID, strategy, false)
	}
	a.ensureRun()
	request := inputClaimRequested{turnID: turnID, strategy: strategy, result: make(chan inputClaimResult, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return turnInput{}, false, session.ErrClosed
	}
	select {
	case outcome := <-request.result:
		if outcome.input.historyNotificationPending {
			message := outcome.input.message()
			a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
			outcome.input.historyNotificationPending = false
		}
		return outcome.input, outcome.delivered, outcome.err
	case <-a.owner.done:
		return turnInput{}, false, session.ErrClosed
	}
}
