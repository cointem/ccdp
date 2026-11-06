package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// runTurn processes one user message through the full agent loop:
// Infer → ToolDispatch → ApprovalGate → Compact.
func (a *Agent) runTurn(input turnInput) {
	var turnErr error
	a.mu.Lock()
	startedAt := a.turn.workStartedAt
	turnCtx := a.turn.turnCtx
	a.mu.Unlock()
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	defer a.lifecycle.turnWG.Done()
	// Completion is enqueued before Done. The owner retains the busy reservation
	// through terminal publication; Close drains the completion lane after Wait.
	defer a.turnFinished()
	text := input.Text
	userMessage := input.message()
	if input.historyNotificationPending {
		a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(userMessage.Role), Content: userMessage.Content})
	}
	// Turn lifecycle facts are admitted before hooks, history publication, or
	// any provider/tool side effect. The first model step is reserved here; a
	// later beginStep uses the same monotonically increasing sequence.
	a.mu.Lock()
	turnSeq := a.sequences.turnSeq
	firstStep := a.sequences.stepSeq + 1
	a.mu.Unlock()
	turnID := fmt.Sprintf("turn-%d", turnSeq)
	turnStarted := false
	if err := a.startTurnJournal(turnID, fmt.Sprintf("step-%d", firstStep), string(input.ID)); err != nil {
		a.failTurnPersistence(err)
		return
	}
	turnStarted = true
	defer func() {
		if !turnStarted {
			return
		}
		if err := a.finishTurnOutcome(turnSeq, turnErr, time.Since(startedAt)); err != nil {
			a.failTurnPersistence(err)
		}
	}()

	finishCheckpoint, checkpointErr := a.beginAutomaticCheckpoint(turnCtx, turnID, text, userMessage.ID)
	if checkpointErr != nil {
		if errors.Is(checkpointErr, context.Canceled) || errors.Is(checkpointErr, context.DeadlineExceeded) {
			return
		}
		turnErr = fmt.Errorf("checkpoint: %w", checkpointErr)
		a.emit(protocol.EventView{Kind: protocol.EventError, Text: turnErr.Error()})
		return
	}
	defer func() {
		// The model has finished. Snapshot sealing and session persistence can
		// take time, but must not keep advertising a live response stream.
		a.setPhase(protocol.PhaseFinalizing)
		finishCheckpoint()
	}()

	// Preparation and hooks share the cancellable lifetime of the turn.
	hookCtx := turnCtx
	if !input.goalContinuation {
		ho := a.runHookWithJournal(hookCtx, hooks.EventUserPromptSubmit, func(hctx context.Context) hooks.Output {
			return a.services.hooks.UserPromptSubmit(hctx, text)
		})
		// A hook killed by user cancellation is a cancelled turn, not a hook
		// policy rejection. Durable recording failures retain their failure gate.
		if hookCtx.Err() != nil {
			return
		}
		if ho.Continue != nil && !*ho.Continue {
			a.cancelWorkFromWorker(ho.Reason)
		}
		if ho.Decision == hooks.DecisionBlock || ho.Decision == hooks.DecisionDeny {
			reason := ho.Reason
			if reason == "" {
				reason = "blocked by UserPromptSubmit hook"
			}
			a.emit(protocol.EventView{Kind: protocol.EventUserMessage, Text: text, MessageID: userMessage.ID})
			turnErr = errors.New(reason)
			a.emit(protocol.EventView{Kind: protocol.EventError, Text: reason})
			return
		} else if ho.Continue != nil && !*ho.Continue {
			// {"continue": false} from a hook stops the agent loop entirely
			// (Claude Code hook semantics).
			reason := ho.Reason
			if reason == "" {
				reason = "stopped by UserPromptSubmit hook"
			}
			a.emit(protocol.EventView{Kind: protocol.EventUserMessage, Text: text, MessageID: userMessage.ID})
			turnErr = errors.New(reason)
			a.emit(protocol.EventView{Kind: protocol.EventError, Text: reason})
			return
		} else if ho.AdditionalContext != "" {
			// Hook-provided context rides along as a system message.
			if err := a.appendHistory(messages.Message{
				Role: messages.RoleSystem, Content: ho.AdditionalContext, CreatedAt: time.Now(),
			}); err != nil {
				a.failTurnPersistence(err)
				return
			}
		}

	}
	if !input.goalContinuation {
		a.emit(protocol.EventView{Kind: protocol.EventUserMessage, Text: text, MessageID: userMessage.ID})
	}
	if !input.historyAppended {
		if err := a.appendHistory(userMessage); err != nil {
			a.failTurnPersistence(err)
			return
		}
	}

	steps := 0
	continueCount := 0
	preparation := stepPreparation{}
	for {
		if a.interrupted() {
			a.postTurnNote(turnNoteRecorded{interrupted: true})
			a.emitStatus("turn interrupted")
			return
		}
		if !a.goalBeforeStep() {
			a.reportStoppedGoal()
			return
		}
		cfg := a.configSnapshot()
		if cfg.MaxTurns > 0 && steps >= cfg.MaxTurns {
			turnErr = &turnStopError{status: protocol.TurnLimited, reason: fmt.Sprintf("model step limit reached (%d); task completion was not confirmed", cfg.MaxTurns)}
			a.emitStatus("%s", turnErr)
			return
		}
		a.setPhase(protocol.PhasePreparing)
		prepared, err := a.prepareStep(turnCtx, &preparation)
		if err != nil {
			if errors.Is(err, errGoalStopped) {
				a.reportStoppedGoal()
				return
			}
			if a.interrupted() || turnCtx.Err() != nil && errors.Is(err, turnCtx.Err()) {
				return
			}
			var inputErr *stepInputError
			if errors.As(err, &inputErr) {
				turnErr = err
				a.emit(protocol.EventView{Kind: protocol.EventError, Text: err.Error()})
			} else {
				a.failTurnPersistence(err)
			}
			return
		}
		steps++
		a.emitStatus("thinking…")
		outcome := a.runStep(turnCtx, prepared, &continueCount)
		if outcome.action == stepRecoverContext && !preparation.contextRecovered {
			preparation.contextRecovered = true
			changed, recoveryErr := a.prepareContextForRequest(turnCtx, prepared.request, true, prepared.runtime)
			if a.interrupted() || turnCtx.Err() != nil {
				if a.interrupted() {
					a.postTurnNote(turnNoteRecorded{interrupted: true})
				}
				return
			}
			if errors.Is(recoveryErr, errGoalStopped) {
				a.reportStoppedGoal()
				return
			}
			if recoveryErr != nil {
				outcome.err = recoveryErr
			}
			if recoveryErr == nil && changed {
				continue
			}
		}
		if outcome.err != nil {
			turnErr = outcome.err
			if outcome.action == stepRecoverContext {
				a.emit(protocol.EventView{Kind: protocol.EventError, Text: outcome.err.Error()})
			}
			return
		}
		if outcome.action == stepRetry {
			// Model fallback rebuilds the request, but does not consume new input.
			preparation.compactChecked = false
			continue
		}
		if outcome.action == stepContinue {
			preparation = stepPreparation{}
			continue
		}
		if outcome.action == stepWait && a.children.childState == nil {
			// runStep has released every frozen lease before the parent waits.
			reason, err := a.waitAgentEvent(turnCtx)
			if err != nil {
				if turnCtx.Err() == nil || !errors.Is(err, turnCtx.Err()) {
					turnErr = err
				}
				return
			}
			if reason != "no_pending_work" {
				preparation = stepPreparation{}
				continue
			}
		}
		return
	}
}
