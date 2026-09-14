package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ccdp/internal/events"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

// SubagentDefaultSystem is the default system prompt for a Task child. Child
// sessions do not reload project instructions; the parent already froze the
// effective configuration and the child constructor appends the usual
// workspace/runtime context.
const SubagentDefaultSystem = `You are a ccdp sub-agent, an independent worker launched by the main agent.
You share the main agent's workspace and a frozen permission snapshot, but
have your own session, history, resources and cancellation boundary. Mutable
project hooks, MCP startup and memory are not inherited.

Operating rules:
- Complete the assigned task independently. Tool approvals are routed to the
  human when an interactive approval channel is enabled. In headless mode,
  calls that require approval are denied; report the limitation and continue
  when possible. Never treat an approval as overriding inherited hard denies.
- The parent's brief is self-contained: work from what it says and verify
  facts with the tools available in this child session.
- Read before writing. Prefer small, verifiable changes and report evidence.
- Stay inside the scope of the brief. Never launch another Task child.

Reporting rules:
- End with a concise final answer the parent can act on directly: what you did,
  files changed, evidence (commands/tests), and anything unfinished.
- Never claim to have performed actions you did not perform.`

// maxChildSteps is retained as a compatibility cap for callers that inspect
// the old package constant. The actual loop is Agent.runTurn; child config is
// capped to the same number by childOptionsFromParent/provider construction.
const maxSubagentSteps = maxChildTurns

func (a *Agent) runSubagents(tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
	return a.runManagedTasks(tasks, "")
}

func (a *Agent) runManagedTasks(tasks []tools.SubagentTask, callID string) ([]tools.SubagentResult, error) {
	results := make([]tools.SubagentResult, len(tasks))
	if len(tasks) == 0 {
		return results, nil
	}
	if a != nil && a.childState != nil {
		for i, task := range tasks {
			results[i] = tools.SubagentResult{Index: i, Description: task.Description, Error: "nested Task is not available"}
		}
		return results, nil
	}

	ctx := parentTurnContext(a)
	runs := make([]*managedRun, len(tasks))
	for i, task := range tasks {
		results[i] = tools.SubagentResult{Index: i, Description: task.Description}
		r, err := a.supervisor.launch(a, ctx, task, childPurposeTask, callID, i)
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		runs[i] = r
		r.mu.Lock()
		results[i].SessionID = string(r.fact.Child.SessionID)
		results[i].RunID = string(r.fact.Child.Run.ID)
		r.mu.Unlock()
	}
	for i, r := range runs {
		if r == nil {
			continue
		}
		if tasks[i].WaitPolicy == "notify" {
			results[i].Output = fmt.Sprintf("Background child %s run %s accepted; completion will be delivered to this session.", results[i].SessionID, results[i].RunID)
			continue
		}
		<-r.done
		r.mu.Lock()
		results[i].Output = r.fact.Child.Run.Output
		results[i].Error = errString(r.err)
		r.mu.Unlock()
	}
	return results, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runSubagent is the Task callback installed in tools.Context. It launches a
// real child Agent and drives it through Submit + Watch + runTurn + Close.
func (a *Agent) runSubagent(description, systemPrompt string) (string, error) {
	if a != nil && a.childState != nil {
		return "", errors.New("nested Task is not available")
	}
	ctx := parentTurnContext(a)
	return a.runTaskChild(ctx, description, systemPrompt, slotsForParent(a))
}

// runTaskChild is shared by the single and batch Task paths. Keeping the
// parent lifecycle hooks here means a batch item has exactly the same
// SubagentStart/Stop semantics as a single Task call; the guardian path never
// enters this helper and therefore never triggers Task hooks.
func (a *Agent) runTaskChild(ctx context.Context, description, systemPrompt string, slots *childSlots) (out string, err error) {
	if a != nil && a.childState != nil {
		return "", errors.New("nested Task is not available")
	}
	out, err = a.runChildWithSlot(ctx, description, systemPrompt, childPurposeTask, slots)
	return out, err
}

// runGuardianChild uses the same isolated lifecycle as Task but with the
// guardian purpose. It intentionally does not route through runSubagent: doing
// so would give a guardian the Task policy and make nested delegation possible.
func (a *Agent) runGuardianChild(description string) (string, error) {
	if a != nil && a.childState != nil {
		return "", errors.New("nested guardian is not available")
	}
	return a.runChildWithSlot(parentTurnContext(a), description, GuardianSystem, childPurposeGuardian, slotsForParent(a))
}

func (a *Agent) runChildWithSlot(ctx context.Context, description, systemPrompt, purpose string, slots *childSlots) (string, error) {
	if a == nil {
		return "", errors.New("child: nil parent")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.runChild(ctx, description, systemPrompt, purpose)
}

func (a *Agent) runChild(ctx context.Context, description, systemPrompt, purpose string) (string, error) {
	description = strings.TrimSpace(description)
	if description == "" {
		return "", errors.New("child: description is required")
	}
	r, err := a.supervisor.launch(a, ctx, tools.SubagentTask{Description: description, SystemPrompt: systemPrompt}, purpose, "", 0)
	if err != nil {
		return "", err
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fact.Child.Run.Output, r.err
}

// driveChild submits one user input and observes the typed Watch stream until
// the child turn's terminal event. It deliberately does not call child
// executeTool/Stream directly: all admission, truncation, cancellation,
// budget and persistence logic remains in Agent.runTurn.
func driveChild(child *Agent, ctx context.Context, description string) (string, error) {
	if child == nil {
		return "", errors.New("child: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Capture the terminal identity that existed before Submit. A newly
	// created child normally has no LastTurn, but keeping this boundary makes
	// resync recovery safe if a caller ever supplies a resumed child.
	before, beforeErr := child.Snapshot(ctx)
	if beforeErr != nil {
		return "", beforeErr
	}
	priorTurn := protocol.TurnID("")
	if before.LastTurn != nil {
		priorTurn = before.LastTurn.TurnID
	}
	sub, err := child.Watch(ctx, protocol.Cursor{})
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Close() }()
	inputID := protocol.InputID(nextRuntimeID("child-input"))
	cmd := protocol.NewSubmitInput(protocol.CommandID(nextRuntimeID("child-command")), protocol.SessionID(child.SessionID()), inputID, description, protocol.InputSteer)
	receipt, err := child.Submit(ctx, cmd)
	if err != nil {
		return "", err
	}
	if receipt.Rejected() {
		if receipt.Error != nil {
			return "", receipt.Error
		}
		return "", errors.New("child input was rejected")
	}

	var output strings.Builder
	var terminalErr string
	resyncs := 0
	for {
		select {
		case <-ctx.Done():
			return childOutput(child, output.String()), ctx.Err()
		case update, ok := <-sub.Updates():
			if !ok {
				return childOutputAfterWatchClose(child, output.String(), terminalErr)
			}
			if update.Type == protocol.UpdateResyncRequired {
				// A watcher closes itself after publishing this marker. Snapshot is
				// the source of truth; only re-watch when the turn is still active.
				_ = sub.Close()
				view, snapshotErr := child.Snapshot(context.Background())
				if snapshotErr != nil {
					return childOutput(child, output.String()), snapshotErr
				}
				if submittedTurnDone(view, priorTurn) {
					return childOutcomeFromView(child, output.String(), terminalErr, view)
				}
				if resyncs >= maxChildWatchResyncs {
					return childOutput(child, output.String()), errors.New("child watch resync limit exceeded")
				}
				resyncs++
				sub, err = child.Watch(ctx, update.Cursor)
				if err != nil {
					return childOutput(child, output.String()), err
				}
				continue
			}
			if update.Snapshot != nil && submittedTurnDone(*update.Snapshot, priorTurn) {
				return childOutcomeFromView(child, output.String(), terminalErr, *update.Snapshot)
			}
			if update.Event == nil {
				continue
			}
			ev := update.Event
			switch ev.Kind {
			case protocol.EventStream:
				appendChildOutput(&output, ev.Text)
			case protocol.EventError:
				if ev.Error != "" {
					terminalErr = ev.Error
				} else if ev.Text != "" {
					terminalErr = ev.Text
				}
			case protocol.EventTurnDone:
				view, snapshotErr := child.Snapshot(context.Background())
				if snapshotErr != nil {
					return childOutput(child, output.String()), snapshotErr
				}
				return childOutcomeFromView(child, output.String(), terminalErr, view)
			}
		}
	}
}

const (
	maxChildOutputChars  = 256 * 1024
	maxChildWatchResyncs = 3
)

// appendChildOutput bounds transient stream accumulation. The final answer is
// rebuilt from child history, but a provider that emits an enormous stream
// must not grow this parent-owned builder without limit while it is running.
func appendChildOutput(output *strings.Builder, text string) {
	if output == nil || output.Len() >= maxChildOutputChars || text == "" {
		return
	}
	remaining := maxChildOutputChars - output.Len()
	if len(text) > remaining {
		text = truncateUTF8Bytes(text, remaining)
	}
	_, _ = output.WriteString(text)
}

func submittedTurnDone(view protocol.SessionView, priorTurn protocol.TurnID) bool {
	if view.LastTurn == nil || view.LastTurn.Status == protocol.TurnRunning {
		return false
	}
	return priorTurn == "" || view.LastTurn.TurnID != priorTurn
}

func childOutcomeFromView(child *Agent, output, terminalErr string, view protocol.SessionView) (string, error) {
	result := childOutput(child, output)
	if view.LastTurn != nil {
		switch view.LastTurn.Status {
		case protocol.TurnFailed:
			if view.LastTurn.Error != "" {
				return result, errors.New(view.LastTurn.Error)
			}
			if terminalErr != "" {
				return result, errors.New(terminalErr)
			}
			return result, errors.New("child turn failed")
		case protocol.TurnCancelled:
			// Preserve errors.Is(err, context.Canceled) for callers that need to
			// distinguish an interrupted child from a failed one.
			return result, context.Canceled
		}
	}
	if terminalErr != "" {
		return result, errors.New(terminalErr)
	}
	return childBudgetOutcome(child, result, view)
}

// childOutput prefers the final assistant message in the durable/in-memory
// history. Stream events are intentionally only a fallback: a Watch can be
// resynchronized or close after dropping transient deltas, while history is
// the recoverable result published by the runtime.
func childOutput(child *Agent, streamed string) string {
	if child != nil {
		history := child.History()
		for i := len(history) - 1; i >= 0; i-- {
			message := history[i]
			if message.Role == messages.RoleAssistant && len(message.ToolCalls) == 0 && message.Content != "" {
				return truncateResult(message.Content, maxChildOutputChars)
			}
		}
	}
	return truncateResult(streamed, maxChildOutputChars)
}

func childOutputAfterWatchClose(child *Agent, output, terminalErr string) (string, error) {
	view, err := child.Snapshot(context.Background())
	if err != nil {
		return childOutput(child, output), err
	}
	if view.Busy || (view.LastTurn != nil && view.LastTurn.Status == protocol.TurnRunning) {
		return childOutput(child, output), errors.New("child watch ended before turn completed")
	}
	return childOutcomeFromView(child, output, terminalErr, view)
}

func childBudgetOutcome(child *Agent, output string, view protocol.SessionView) (string, error) {
	if child == nil {
		return truncateResult(output, maxChildOutputChars), nil
	}
	child.mu.Lock()
	cfg := cloneConfig(child.cfg)
	child.mu.Unlock()
	history := child.History()
	if cfg.MaxBudgetUSD > 0 && view.Usage.Cost >= cfg.MaxBudgetUSD {
		return childOutput(child, output), fmt.Errorf("child exceeded budget ($%.2f of $%.2f)", view.Usage.Cost, cfg.MaxBudgetUSD)
	}
	if cfg.MaxTurns > 0 && len(history) > 0 {
		modelCalls := 0
		latestAssistantHasTools := false
		for i := len(history) - 1; i >= 0; i-- {
			message := history[i]
			if message.Role != messages.RoleAssistant {
				continue
			}
			if modelCalls == 0 {
				latestAssistantHasTools = len(message.ToolCalls) > 0
			}
			if len(message.ToolCalls) > 0 {
				modelCalls++
			}
		}
		if latestAssistantHasTools && modelCalls >= cfg.MaxTurns {
			return childOutput(child, output), fmt.Errorf("child exceeded %d turns", cfg.MaxTurns)
		}
	}
	return childOutput(child, output), nil
}

// recordChildUsage emits one parent usage update for the completed child. It
// deliberately adds the child's already-priced cost and never changes the
// parent's token baseline.
func (a *Agent) recordChildUsage(u Usage) error {
	if a == nil || (u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedTokens == 0 && u.Cost == 0 && u.TurnCount == 0) {
		return nil
	}
	// Keep the durable UsageChanged commit and the live projection in one
	// ordered critical section. A parent model request may be committing its
	// own AttemptFinished fact at the same time; persistMu prevents either
	// absolute usage value from being lost or committed out of order.
	a.persistMu.Lock()
	p := a.persistenceHandle()
	if p == nil {
		err := errors.New("agent: session persistence is unavailable")
		a.persistMu.Unlock()
		a.markPersistenceFailure(err)
		return err
	}
	if err := a.persistenceFailure(); err != nil {
		a.persistMu.Unlock()
		a.markPersistenceFailure(err)
		return err
	}
	a.mu.Lock()
	candidate := a.usage
	candidate.InputTokens += u.InputTokens
	candidate.OutputTokens += u.OutputTokens
	candidate.CachedTokens += u.CachedTokens
	candidate.Cost += u.Cost
	candidate.TurnCount += u.TurnCount
	a.mu.Unlock()
	if err := p.persistUsage(candidate); err != nil {
		a.persistMu.Unlock()
		a.markPersistenceFailure(err)
		return err
	}
	a.mu.Lock()
	a.usage = candidate
	a.mu.Unlock()
	// Do not hold persistMu while publishing events. An in-process subscriber
	// may synchronously call Save/another persistence helper; the durable
	// commit and live swap above are already complete at this point.
	a.persistMu.Unlock()
	a.emit(Event{Type: EventUsage, Usage: &candidate})
	if a.evbus != nil {
		a.evbus.Emit(events.TopicUsageUpdated, events.UsageEvent{
			InputTokens: candidate.InputTokens, OutputTokens: candidate.OutputTokens,
			TurnCount: candidate.TurnCount, Cost: candidate.Cost,
		})
	}
	return nil
}
