package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/sessionview"
	"ccdp/internal/tooling"
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
		a.emit(Event{Type: EventError, Text: turnErr.Error()})
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
			a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
			turnErr = errors.New(reason)
			a.emit(Event{Type: EventError, Text: reason})
			return
		} else if ho.Continue != nil && !*ho.Continue {
			// {"continue": false} from a hook stops the agent loop entirely
			// (Claude Code hook semantics).
			reason := ho.Reason
			if reason == "" {
				reason = "stopped by UserPromptSubmit hook"
			}
			a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
			turnErr = errors.New(reason)
			a.emit(Event{Type: EventError, Text: reason})
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
		a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
	}
	if !input.historyAppended {
		if err := a.appendHistory(userMessage); err != nil {
			a.failTurnPersistence(err)
			return
		}
	}

	steps := 0         // model-call iterations in this turn (--max-turns)
	continueCount := 0 // automatic continuation for token-limited replies
	for {
		if a.interrupted() {
			// Flag the interruption so the next request tells the model that
			// tools may have partially executed (Codex's INTERRUPTED_GUIDANCE).
			a.postTurnNote(turnNoteRecorded{interrupted: true})
			a.emitStatus("turn interrupted")
			return
		}
		if !a.goalBeforeStep() {
			a.reportStoppedGoal()
			return
		}
		// A collaboration wake resumes request preparation in this same turn.
		// Waiting is a runtime phase, so snapshots must stop advertising it here.
		a.setPhase(protocol.PhasePreparing)
		if err := a.consumeAgentMessages(); err != nil {
			a.failTurnPersistence(err)
			return
		}
		// Compaction gate (Codex: compact between turns).
		if a.needsCompact() {
			a.emitStatus("context near limit, compacting…")
			a.compact()
			if a.interrupted() {
				return
			}
			if !a.goalBeforeStep() {
				a.reportStoppedGoal()
				return
			}
		}

		// -- Infer --
		// --max-turns: stop calling the model after the cap; report the
		// accumulated state so far instead of silently hanging.
		cfg := a.configSnapshot()
		if cfg.MaxTurns > 0 && steps >= cfg.MaxTurns {
			a.emitStatus("max turns reached (%d) — stopping", cfg.MaxTurns)
			return
		}
		steps++

		a.emitStatus("thinking…")
		step, stepErr := a.beginStepChecked()
		if stepErr != nil {
			var inputErr *stepInputError
			if errors.As(stepErr, &inputErr) {
				turnErr = inputErr
				a.emit(Event{Type: EventError, Text: inputErr.Error()})
			} else {
				a.failTurnPersistence(stepErr)
			}
			return
		}
		stepReleased := false
		releaseStep := func() {
			if stepReleased {
				return
			}
			stepReleased = true
			releaseStepLease(step)
		}
		req, requestErr := a.buildRequestForStepChecked(step)
		if requestErr != nil {
			releaseStep()
			var inputErr *stepInputError
			if errors.As(requestErr, &inputErr) {
				turnErr = inputErr
				a.emit(Event{Type: EventError, Text: inputErr.Error()})
			} else {
				a.failTurnPersistence(requestErr)
			}
			return
		}
		call, prepareErr := a.prepareProviderCall(step.binding, req)
		if prepareErr != nil {
			releaseStep()
			detail := safeAttemptError(prepareErr, "", step.binding.endpoint)
			if detail == "" {
				detail = "request preparation failed"
			}
			turnErr = fmt.Errorf("request preparation failed: %s", detail)
			a.emit(Event{Type: EventError, Text: turnErr.Error()})
			return
		}

		var streamed strings.Builder
		var segments []messages.StreamSegment
		segmentKind := ""
		addSegment := func(kind, delta string) {
			if delta == "" {
				segmentKind = ""
				return
			}
			if segmentKind != kind {
				segments = append(segments, messages.StreamSegment{Kind: kind})
				segmentKind = kind
			}
			segments[len(segments)-1].Bytes += len(delta)
		}
		var res llm.StreamResult
		var streamErr error
		a.setPhase(protocol.PhaseStreaming)
		streamDone := make(chan struct{})
		go func() {
			purpose := "main"
			if a.children.childState != nil && a.children.childState.purpose != "" {
				purpose = a.children.childState.purpose
			}
			metadata := requestJournalMetadata{
				Config: step.cfg, SettingsRevision: step.settingsRev,
				ContextRevision: step.contextRev, CatalogVersion: step.catalogVersion,
				Turn: step.turn, Step: step.step, HistoryLen: len(step.history),
			}
			r, err := a.streamPrepared(turnCtx, purpose, call, metadata, func(delta string) {
				addSegment("assistant", delta)
				streamed.WriteString(delta)
				a.emit(Event{Type: EventStream, Text: delta})
			}, func(delta string) {
				addSegment("thinking", delta)
				a.emit(Event{Type: EventReasoning, Text: delta})
			})
			res = r
			streamErr = err
			close(streamDone)
		}()
		<-streamDone
		// Stop the streaming phase before committing the answer or processing
		// its tool calls. A completed provider stream is no longer live output.
		a.setPhase(protocol.PhasePreparing)

		if streamErr != nil {
			if a.handleStreamError(streamErr, releaseStep, len(segments) == 0 && res.Text == "" && res.Reasoning == "") {
				continue // switched to the fallback model; retry this step
			}
			if !a.interrupted() && !errors.Is(streamErr, errGoalStopped) {
				turnErr = fmt.Errorf("LLM error: %w", streamErr)
			}
			return
		}

		callText := res.Text
		if callText == "" {
			// A provider may return the aggregate only (without invoking the
			// preview sink). Keep the old sink-only compatibility for providers
			// that stream deltas but return an empty aggregate.
			callText = streamed.String()
		}
		var calls []messages.ToolCall
		var toolCallErr error
		calls, toolCallErr = tooling.DecodeCalls(res.ToolCalls)
		if toolCallErr != nil {
			// Record the assistant prose, but never dispatch a malformed call.
			// In particular, do not let malformed/null/array arguments become
			// the historical {} default that could trigger a side effect.
			if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), messages.Message{Role: messages.RoleAssistant, Content: callText}); err != nil {
				releaseStep()
				a.failTurnPersistence(err)
				return
			}
			releaseStep()
			turnErr = fmt.Errorf("invalid tool call: %w", toolCallErr)
			a.emit(Event{Type: EventError, Text: turnErr.Error()})
			return
		}
		a.recordGoalResponse(callText, len(calls))
		assistant := messages.AssistantWithTools(callText, calls)
		assistant.ReasoningContent = res.Reasoning
		assistant.ProviderOrigin = res.ProviderOrigin
		assistant.ProviderWire = res.ProviderWire
		assistant.ProviderContent = res.ProviderContent
		if sessionview.ValidStreamSegments(segments, callText, res.Reasoning) {
			assistant.StreamSegments = segments
		}
		if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), assistant); err != nil {
			releaseStep()
			a.failTurnPersistence(err)
			return
		}

		// No tool calls → the assistant answered; turn is done, unless plan
		// mode is on (then the text is a plan awaiting approval), or the reply
		// was cut off by the token limit (then we continue automatically).
		if len(calls) == 0 {

			if a.inPlanMode() {
				if !a.requestPlanApproval(callText) {
					releaseStep()
					a.pauseGoalFromWorker("plan was not approved")
					a.emit(Event{Type: EventStatus, Text: "plan not approved — no changes made"})
					return
				}
				// Approved: exit plan mode and execute the plan step by step.
				if err := a.setPlanMode(false); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				if err := a.appendHistory(messages.Message{
					Role: messages.RoleUser, Content: "The plan above is approved. Execute it now, following the plan step by step. Verify each step.",
					CreatedAt: time.Now(),
				}); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				releaseStep()
				continue
			}
			// The reply hit a provider or configured output limit; ask the
			// model to keep going (bounded, so it cannot loop forever).
			if res.FinishReason == "length" && continueCount < 3 {
				continueCount++
				a.emitStatus("reply hit the token limit — continuing…")
				if err := a.appendHistory(messages.Message{
					Role:      messages.RoleUser,
					Content:   "Continue your reply from where you left off.",
					CreatedAt: time.Now(),
				}); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				releaseStep()
				continue
			}
			// AutoMem: remember the final assistant answer as the turn's outcome.
			if a.children.childState == nil {
				reason, err := a.waitAgentEvent(turnCtx)
				if err != nil {
					releaseStep()
					// Cancellation ends the wait normally; recording it as a
					// failure would override the turn's cancelled outcome.
					if turnCtx.Err() == nil || !errors.Is(err, turnCtx.Err()) {
						turnErr = err
					}
					return
				}
				if reason != "no_pending_work" {
					if reason == "user_input" {
						if _, _, err := a.claimPendingInputAndAppend(fmt.Sprintf("turn-%d", a.currentTurnSeq()), protocol.InputSteer); err != nil {
							releaseStep()
							a.failTurnPersistence(err)
							return
						}
					}
					releaseStep()
					continue
				}
			}
			a.postTurnNote(turnNoteRecorded{summary: callText})
			releaseStep()
			return
		}

		// -- ToolDispatch + ApprovalGate --
		// Parallel dispatch (Codex-style) when configured; results are always
		// appended in call order so the conversation stays deterministic.
		//
		// Pi's failToolCallsFromTruncatedMessage: a reply cut off by the token
		// limit may carry tool calls whose streamed arguments were "salvaged"
		// into plausible-looking but incomplete JSON. Never execute those —
		// hand back an error result so the model re-issues the call with the
		// complete arguments.
		results, deferredToolEvents := a.executeToolCallsForStep(calls, step, res)
		// Every tool_call id MUST get a result message or the next request
		// violates the protocol (providers answer 400). dispatchTools fills
		// synthetic results for calls skipped by an interrupt, so we append
		// all of them unconditionally — never break early here.
		if err := a.persistenceFailure(); err != nil {
			releaseStep()
			a.failTurnPersistence(err)
			return
		}
		for i, result := range results {
			message := messages.NewToolResult(calls[i], result.output, result.isErr)
			a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
		}
		for _, ev := range deferredToolEvents {
			a.emit(ev)
		}

		// Steer (pi's one-at-a-time steering): a user message queued while the
		// turn was busy is injected at this tool-result boundary so the model
		// can react to it inside the running turn. Messages that arrive after
		// the last tool round drain as follow-up turns in turnFinished.
		if next, ok, err := a.claimPendingInputAndAppend(fmt.Sprintf("turn-%d", a.currentTurnSeq()), protocol.InputSteer); err != nil {
			releaseStep()
			a.failTurnPersistence(err)
			return
		} else if ok && next.Strategy == protocol.InputSteer {
			a.emit(Event{Type: EventUserMsg, Text: next.Text, MessageID: next.message().ID})
			a.emitStatus("steered your message into the running turn")
		}
		releaseStep()
		// Loop back to Infer with the tool results appended.
	}
}
