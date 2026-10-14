package agent

import (
	"errors"
	"fmt"

	"ccdp/internal/protocol"
)

// settleTurnLocked records the terminal outcome while the turn still owns the
// lifecycle lock. This is deliberately done before publishState and TurnDone:
// a watcher must never observe Busy=false with a running LastTurn, and an old
// terminal event must not be interpreted as belonging to a newly admitted turn.
// The caller must hold a.mu.
func (a *Agent) settleTurnLocked(turnID protocol.TurnID, saveErr error) {
	if a.lifecycle.lastTurn == nil || a.lifecycle.lastTurn.TurnID != turnID {
		a.lifecycle.lastTurn = &protocol.TurnOutcome{TurnID: turnID, Status: protocol.TurnRunning}
	}
	outcome := a.lifecycle.lastTurn
	switch {
	case saveErr != nil:
		outcome.Status = protocol.TurnFailed
		outcome.Error = "session persistence failed: " + saveErr.Error()
	case a.services.persistenceErr != nil:
		outcome.Status = protocol.TurnFailed
		outcome.Error = "session persistence failed: " + a.services.persistenceErr.Error()
	case outcome.Status == protocol.TurnFailed:
		// Preserve the provider/tool error already observed for this turn.
	case outcome.Status == protocol.TurnCancelled || a.turn.interruptFlag || a.lifecycle.closing || a.lifecycle.closed || a.turn.turnCtx != nil && a.turn.turnCtx.Err() != nil:
		outcome.Status = protocol.TurnCancelled
		outcome.Error = ""
	default:
		outcome.Status = protocol.TurnSucceeded
		outcome.Error = ""
	}
}

// handleStreamError classifies a provider-stream failure and resolves it. It
// returns true when the caller should retry the step against the fallback model
// (the primary client failed once and the fallback budget for this turn is
// unused); false ends the turn, having released the step lease and emitted the
// appropriate error/interruption signal.
func (a *Agent) handleStreamError(streamErr error, releaseStep func(), allowFallback bool) bool {
	if errors.Is(streamErr, errGoalStopped) {
		releaseStep()
		a.reportStoppedGoal()
		return false
	}
	if isRequestJournalFailure(streamErr) {
		releaseStep()
		a.emit(Event{Type: EventError, Text: "request journal error: " + streamErr.Error()})
		return false
	}
	if a.interrupted() {
		releaseStep()
		a.postTurnNote(turnNoteRecorded{interrupted: true})
		a.emitStatus("stream interrupted")
		return false
	}
	// Fallback model (Claude's --fallback-model): one retry on the backup client,
	// then give up. activeModel follows so the request builder sends the fallback
	// model name to the fallback
	// endpoint (req.Model comes from activeModelSnapshot).
	if allowFallback {
		if model := a.requestFallback(); model != "" {
			releaseStep()
			a.emitStatus("primary model failed (%v) — falling back to %s", streamErr, model)
			return true
		}
	}
	releaseStep()
	a.emit(Event{Type: EventError, Text: "LLM error: " + streamErr.Error()})
	return false
}

// turnFinished settles worker-owned memory and goal work, then transfers the
// terminal boundary to the owner without depending on the execution context.
func (a *Agent) turnFinished() {
	request := turnCleanupRequested{result: make(chan turnCleanupResult, 1)}
	a.ensureRun()
	a.owner.control <- request
	result := <-request.result
	// The worker releases its reservation only after reliable terminal handoff.
	a.owner.control <- turnCompleted{sequence: result.sequence, saveErr: result.err}
}

// completeTurn is called only by the command owner. Busy remains reserved
// until this handler publishes the terminal boundary and claims the next input.
func (a *Agent) completeTurn(completion turnCompleted) {
	a.mu.Lock()
	if completion.sequence != a.sequences.turnSeq || !a.turn.busy {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	completedTurnSeq, saveErr := completion.sequence, completion.saveErr
	turnID := protocol.TurnID(fmt.Sprintf("%d", completedTurnSeq))
	// Keep the owner in a terminal hand-off state while LastTurn, phase, and
	// the explicit terminal event are made consistent. New Submit calls see
	// settling and are durably queued instead of racing this event.
	a.mu.Lock()
	a.lifecycle.settling = true
	a.turn.busy = false
	if a.lifecycle.closing || a.lifecycle.closed {
		a.lifecycle.phase = protocol.PhaseStopping
	} else {
		a.lifecycle.phase = protocol.PhaseIdle
	}
	a.settleTurnLocked(turnID, saveErr)
	a.mu.Unlock()
	if saveErr != nil {
		a.emit(Event{Type: EventError, TurnID: string(turnID), Text: "session persistence failed: " + saveErr.Error()})
	}
	// If a queued input was present at the terminal boundary, publishTerminal
	// keeps busy=true as a reservation and returns true. If none was present,
	// it has already made the session idle and published the complete boundary;
	// the old turn must not write state again after a new command is admitted.
	if !a.publishTerminal(turnID) {
		return
	}

	a.launchQueuedTurn(completedTurnSeq)
}

// idleAfterTurn resets the owner to a non-busy terminal phase under the lock.
func (a *Agent) idleAfterTurn() {
	a.mu.Lock()
	a.turn.busy = false
	a.lifecycle.settling = false
	if a.lifecycle.closing || a.lifecycle.closed {
		a.lifecycle.phase = protocol.PhaseStopping
	} else {
		a.lifecycle.phase = protocol.PhaseIdle
	}
	a.mu.Unlock()
}

// launchQueuedTurn claims the oldest queued input and starts it as the next
// turn under the owner lock. The caller must already hold the busy reservation
// (from publishTerminal or a compaction reservation) so a competing turn cannot
// start. When no input is available, or cancellation/persistence stalls it, the
// session is left idle and false is returned.
func (a *Agent) launchQueuedTurn(completedTurnSeq uint64) bool {
	nextTurnID := fmt.Sprintf("turn-%d", completedTurnSeq+1)
	next, ok, err := a.claimPendingInput(nextTurnID, "", true)
	if err != nil {
		a.markPersistenceFailure(err)
		a.idleAfterTurn()
		a.emit(Event{Type: EventError, Text: "queued input delivery failed: " + err.Error()})
		a.publishState()
		return false
	}
	if !ok {
		if a.launchGoalTurn() {
			return true
		}
		a.idleAfterTurn()
		a.publishState()
		return false
	}
	a.mu.Lock()
	if a.lifecycle.closing || a.lifecycle.closed || a.turn.interruptFlag || a.services.persistenceErr != nil {
		// The durable delivery is retained in history, but cancellation wins
		// over starting another provider request. The input is not re-queued.
		a.turn.busy = false
		a.lifecycle.settling = false
		if !a.lifecycle.closing && !a.lifecycle.closed {
			a.lifecycle.phase = protocol.PhaseIdle
		}
		a.mu.Unlock()
		a.publishState()
		return false
	}
	a.beginTurnLocked("", next.Text)
	a.mu.Unlock()
	a.publishState()
	go a.runTurn(next)
	return true
}
