package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/sessionview"
	"ccdp/internal/tooling"
)

// A retry keeps the same input boundary. A new response/tool round starts a
// new boundary, allowing at most one steer before its next provider request.
type stepPreparation struct {
	inputsClaimed      bool
	steerClaimed       bool
	compactChecked     bool
	attachmentsChecked bool
	contextRecovered   bool
}

type preparedStep struct {
	runtime stepRuntime
	request llm.CompletionRequest
	call    llm.PreparedCall
}

type stepAction uint8

const (
	stepDone stepAction = iota
	stepContinue
	stepRetry
	stepWait
	stepRecoverContext
)

type stepOutcome struct {
	action stepAction
	err    error
}

// prepareStep is the only running-turn inbox consumer. Collaboration messages
// are drained first; a follow-up at the head of the user queue blocks later
// steers. Compaction can take time, so check that boundary again before freezing
// the final request, without consuming a second steer.
func (a *Agent) prepareStep(ctx context.Context, state *stepPreparation) (preparedStep, error) {
	if !state.inputsClaimed {
		if err := a.consumeStepInputs(state); err != nil {
			return preparedStep{}, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return preparedStep{}, err
		}
		if !a.goalBeforeStep() {
			return preparedStep{}, errGoalStopped
		}
		step, err := a.beginStepChecked()
		if err != nil {
			return preparedStep{}, err
		}
		if !state.attachmentsChecked && historyAttachmentBytes(step.history) > maxAttachmentBytes {
			state.attachmentsChecked = true
			a.releaseStepContext(step)
			if err := a.compactContextMode(ctx, false); err != nil {
				return preparedStep{}, &stepInputError{err: fmt.Errorf("prepare attachments: %w", err)}
			}
			if !state.inputsClaimed {
				if err := a.consumeStepInputs(state); err != nil {
					return preparedStep{}, err
				}
			}
			continue
		}
		req, err := a.buildRequestForStepChecked(step)
		if err != nil {
			a.releaseStepContext(step)
			return preparedStep{}, err
		}
		if !state.compactChecked {
			state.compactChecked = true
			if a.needsContextPreparation(req, step) {
				a.releaseStepContext(step)
				a.emitStatus("context near limit, compacting…")
				if _, err := a.prepareContextForRequest(ctx, req, false, step); err != nil {
					return preparedStep{}, &stepInputError{err: fmt.Errorf("prepare context: %w", err)}
				}
				if !state.inputsClaimed {
					if err := a.consumeStepInputs(state); err != nil {
						return preparedStep{}, err
					}
				}
				continue
			}
		}
		call, err := a.prepareProviderCall(step.binding, req)
		if err != nil {
			a.releaseStepContext(step)
			if errors.Is(err, llm.ErrContextWindowExceeded) && !state.contextRecovered {
				state.contextRecovered = true
				changed, recoveryErr := a.prepareContextForRequest(ctx, req, true, step)
				if recoveryErr != nil {
					return preparedStep{}, &stepInputError{err: fmt.Errorf("prepare context: %w", recoveryErr)}
				}
				if changed {
					if !state.inputsClaimed {
						if err := a.consumeStepInputs(state); err != nil {
							return preparedStep{}, err
						}
					}
					continue
				}
			}
			detail := safeAttemptError(err, "", step.binding.endpoint)
			if detail == "" {
				detail = "request preparation failed"
			}
			return preparedStep{}, &stepInputError{err: fmt.Errorf("request preparation failed: %s", detail)}
		}
		state.inputsClaimed = true
		return preparedStep{runtime: step, request: req, call: call}, nil
	}
}

func (a *Agent) consumeStepInputs(state *stepPreparation) error {
	if err := a.consumeAgentMessages(); err != nil {
		return err
	}
	if state.steerClaimed {
		return nil
	}
	next, ok, err := a.claimPendingInputAndAppend(fmt.Sprintf("turn-%d", a.currentTurnSeq()), protocol.InputSteer)
	if err != nil {
		return err
	}
	if ok {
		state.steerClaimed = true
		a.emit(Event{Type: EventUserMsg, Text: next.Text, MessageID: next.message().ID})
		a.emitStatus("steered your message into the running turn")
	}
	return nil
}

// runStep owns the frozen request lease until its response and all tool results
// have been committed. Every return releases it; waiting for child work belongs
// to runTurn, after this function has returned.
func (a *Agent) runStep(ctx context.Context, prepared preparedStep, continueCount *int) stepOutcome {
	step := prepared.runtime
	defer a.releaseStepContext(step)
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
	purpose := "main"
	if a.children.childState != nil && a.children.childState.purpose != "" {
		purpose = a.children.childState.purpose
	}
	metadata := requestJournalMetadata{
		Config: step.cfg, SettingsRevision: step.settingsRev,
		ContextRevision: step.contextRev, CatalogVersion: step.catalogVersion,
		Turn: step.turn, Step: step.step, HistoryLen: len(step.history),
	}
	a.setPhase(protocol.PhaseStreaming)
	res, streamErr := a.streamPrepared(ctx, purpose, prepared.call, metadata, func(delta string) {
		addSegment("assistant", delta)
		streamed.WriteString(delta)
		a.emit(Event{Type: EventStream, Text: delta})
	}, func(delta string) {
		addSegment("thinking", delta)
		a.emit(Event{Type: EventReasoning, Text: delta})
	})
	a.setPhase(protocol.PhasePreparing)
	if streamErr != nil {
		if !a.interrupted() && ctx.Err() != nil && errors.Is(streamErr, ctx.Err()) {
			return stepOutcome{}
		}
		empty := len(segments) == 0 && res.Text == "" && res.Reasoning == ""
		if empty && errors.Is(streamErr, llm.ErrContextWindowExceeded) && !a.interrupted() {
			return stepOutcome{action: stepRecoverContext, err: fmt.Errorf("LLM error: %w", streamErr)}
		}
		if a.handleStreamError(streamErr, empty) {
			return stepOutcome{action: stepRetry}
		}
		if !a.interrupted() && !errors.Is(streamErr, errGoalStopped) {
			return stepOutcome{err: fmt.Errorf("LLM error: %w", streamErr)}
		}
		return stepOutcome{}
	}
	callText := res.Text
	if callText == "" {
		callText = streamed.String()
	}
	calls, toolCallErr := tooling.DecodeCalls(res.ToolCalls)
	if toolCallErr != nil {
		if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), messages.Message{Role: messages.RoleAssistant, Content: callText}); err != nil {
			a.failTurnPersistence(err)
			return stepOutcome{err: err}
		}
		err := fmt.Errorf("invalid tool call: %w", toolCallErr)
		a.emit(Event{Type: EventError, Text: err.Error()})
		return stepOutcome{err: err}
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
		a.failTurnPersistence(err)
		return stepOutcome{err: err}
	}
	if len(calls) == 0 {
		if a.inPlanMode() {
			if !a.requestPlanApproval(callText) {
				a.pauseGoalFromWorker("plan was not approved")
				a.emit(Event{Type: EventStatus, Text: "plan not approved — no changes made"})
				return stepOutcome{}
			}
			if err := a.setPlanMode(false); err != nil {
				a.failTurnPersistence(err)
				return stepOutcome{err: err}
			}
			if err := a.appendHistory(messages.Message{
				Role: messages.RoleUser, Content: "The plan above is approved. Execute it now, following the plan step by step. Verify each step.", CreatedAt: time.Now(),
			}); err != nil {
				a.failTurnPersistence(err)
				return stepOutcome{err: err}
			}
			return stepOutcome{action: stepContinue}
		}
		if res.FinishReason == "length" && *continueCount < 3 {
			*continueCount++
			a.emitStatus("reply hit the token limit — continuing…")
			if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "Continue your reply from where you left off.", CreatedAt: time.Now()}); err != nil {
				a.failTurnPersistence(err)
				return stepOutcome{err: err}
			}
			return stepOutcome{action: stepContinue}
		}
		return stepOutcome{action: stepWait}
	}
	results, deferredEvents, toolErr := a.executeToolCallsForStep(calls, step, res)
	if err := a.persistenceFailure(); err != nil {
		a.failTurnPersistence(err)
		return stepOutcome{err: err}
	}
	if toolErr == nil {
		for i, result := range results {
			message := messages.NewToolResult(calls[i], result.output, result.isErr)
			a.services.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(message.Role), Content: message.Content})
		}
	}
	for _, event := range deferredEvents {
		a.emit(event)
	}
	if toolErr != nil {
		a.emit(Event{Type: EventError, Text: toolErr.Error()})
		return stepOutcome{err: toolErr}
	}
	return stepOutcome{action: stepContinue}
}
