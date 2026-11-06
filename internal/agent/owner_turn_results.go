package agent

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type turnOutcomePrepared struct {
	sequence uint64
	err      error
	duration time.Duration
	result   chan error
}

func (turnOutcomePrepared) ownerControlMessage() {}

type turnCleanupRequested struct{ result chan turnCleanupResult }

func (turnCleanupRequested) ownerControlMessage() {}

type turnCleanupResult struct {
	sequence uint64
	err      error
}

type turnPhaseChanged struct {
	phase protocol.RuntimePhase
	done  chan struct{}
}

func (turnPhaseChanged) ownerControlMessage() {}

type fallbackRequested struct{ result chan string }

func (fallbackRequested) ownerControlMessage() {}

type turnNoteRecorded struct {
	interrupted bool
	done        chan struct{}
}

func (turnNoteRecorded) ownerControlMessage() {}

func (a *Agent) finishTurnOutcome(sequence uint64, turnErr error, duration time.Duration) error {
	request := turnOutcomePrepared{sequence: sequence, err: turnErr, duration: duration, result: make(chan error, 1)}

	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
func (a *Agent) settleTurnOutcome(request turnOutcomePrepared) error {
	a.mu.Lock()
	if request.sequence != a.sequences.turnSeq {
		a.mu.Unlock()
		return fmt.Errorf("turn completion identity changed")
	}
	turnID := protocol.TurnID(fmt.Sprint(request.sequence))
	if a.lifecycle.lastTurn == nil {
		a.lifecycle.lastTurn = &protocol.TurnOutcome{TurnID: turnID, Status: protocol.TurnRunning}
	}
	if request.err != nil {
		a.lifecycle.lastTurn.Status, a.lifecycle.lastTurn.Error = protocol.TurnFailed, request.err.Error()
		var stop *turnStopError
		if errors.As(request.err, &stop) {
			a.lifecycle.lastTurn.Status = stop.status
		}
	}
	a.settleTurnLocked(turnID, nil)
	result := *a.lifecycle.lastTurn
	a.mu.Unlock()
	if result.Status != protocol.TurnSucceeded && a.services.supervisor != nil {
		a.services.supervisor.cancelDelegated(a)
	}
	outcome := "success"
	switch result.Status {
	case protocol.TurnFailed:
		outcome = "error"
	case protocol.TurnCancelled:
		outcome = "cancelled"
	case protocol.TurnLimited, protocol.TurnBlocked:
		outcome = string(result.Status)
	}
	return a.persistTurnFinished(fmt.Sprintf("turn-%d", request.sequence), outcome, result.Error, request.duration)
}

func (a *Agent) prepareTurnCleanup() turnCleanupResult {
	a.mu.Lock()
	if a.turn.turnCancel != nil {
		a.turn.turnCancel()
	}
	a.turn.turnCancel, a.turn.turnCtx = nil, nil
	sequence := a.sequences.turnSeq
	if a.children.childStep != nil && a.children.childStep.turn == sequence {
		a.children.childStep = nil
	}
	a.mu.Unlock()
	a.finishGoalTurnOwned()
	return turnCleanupResult{sequence: sequence}
}

func (a *Agent) selectFallback() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	fallback := a.modelsState.fallbackBinding
	if a.modelsState.usedFallback || fallback.client == nil {
		return ""
	}
	a.modelsState.usedFallback = true
	a.services.client, a.modelsState.activeModel = fallback.client, fallback.model
	fallback.version = a.modelsState.activeBinding.version + 1
	a.modelsState.activeBinding = fallback
	return fallback.model
}
func (a *Agent) requestFallback() string {
	request := fallbackRequested{result: make(chan string, 1)}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return ""
	}
	select {
	case model := <-request.result:
		return model
	case <-a.owner.done:
		return ""
	}
}

func (a *Agent) recordTurnNote(request turnNoteRecorded) {
	a.mu.Lock()
	if request.interrupted {
		a.turnMemory.interruptNote = true
	}
	a.mu.Unlock()
}
func (a *Agent) postTurnNote(request turnNoteRecorded) {
	request.done = make(chan struct{})
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return
	}
	select {
	case <-request.done:
	case <-a.owner.done:
	}
}
