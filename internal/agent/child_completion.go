package agent

import (
	"context"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
	"ccdp/internal/supervision"
)

// Called after the run has joined its workers but before releasing its writer.
// Cancellation is durable so a later continuation cannot drain old inputs.
func (a *Agent) cancelRunInputs(runID string) error {
	a.mu.Lock()
	var facts []session.Event
	for _, input := range a.conversation.durable.PendingInputViews() {
		if input.Strategy == protocol.InputMessage {
			continue
		}
		facts = append(facts, session.InputCancelled{InputID: string(input.ID), Reason: "run settled"})
	}
	a.mu.Unlock()
	if len(facts) == 0 {
		return nil
	}
	if _, err := a.persistenceHandle().CommitEvents("run-cancel-inputs-"+runID, facts...); err != nil {
		return err
	}
	a.mu.Lock()
	for _, event := range facts {
		if cancelled, ok := event.(session.InputCancelled); ok {
			delete(a.inbox.inputAttachments, protocol.InputID(cancelled.InputID))
		}
	}
	a.mu.Unlock()
	return nil
}

func (a *Agent) RunOutput(previous map[string]bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A failed continuation must never return a previous run's answer.
	for i := len(a.historyLocked()) - 1; i >= 0; i-- {
		if a.historyLocked()[i].Role == messages.RoleAssistant && a.historyLocked()[i].Content != "" && !previous[a.historyLocked()[i].ID] {
			return a.historyLocked()[i].Content
		}
	}
	return ""
}

func (a *Agent) RunStatus() supervision.RuntimeStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := supervision.RuntimeStatus{ToolUses: a.toolUses, Usage: protocol.UsageSnapshot{InputTokens: a.runtimeUsageLocked().InputTokens, OutputTokens: a.runtimeUsageLocked().OutputTokens, CachedTokens: a.runtimeUsageLocked().CachedTokens, Cache: a.runtimeUsageLocked().Cache, TurnCount: a.runtimeUsageLocked().TurnCount}}
	if p := a.decisions.pendingApproval; p != nil {
		out.Approval = &protocol.ApprovalView{ID: p.ID, Tool: p.Tool, Reason: p.Reason, Args: approvalArgsView(p), Capabilities: append([]protocol.CapabilityRequest(nil), p.Capabilities...)}
	}
	out.Question = protocol.CloneQuestionRequest(a.decisions.pendingQuestion)
	if p := a.plan.pendingPlan; p != nil {
		out.Plan = &protocol.PlanView{ID: p.ID, Text: p.Plan, Version: planViewVersion}
	}
	return out
}

func (a *Agent) RequestAdmitted(id string) bool {
	p := a.persistenceHandle()
	return p != nil && p.RequestAdmitted(id)
}

func (a *Agent) ReadRunRecords(ctx context.Context) ([]session.Record, journal.ArtifactReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p := a.persistenceHandle()
	if p == nil {
		return nil, nil, session.ErrClosed
	}
	return p.ReadSnapshot()
}

func (a *Agent) PersistenceFailure() error { return a.persistenceFailure() }

type runInputsCancelled struct {
	runID  string
	result chan error
}

func (runInputsCancelled) ownerControlMessage() {}
func (a *Agent) cancelRunInputsFromWorker(runID string) error {
	if a.owner == nil {
		return a.cancelRunInputs(runID)
	}
	request := runInputsCancelled{runID: runID, result: make(chan error, 1)}
	a.ensureRun()
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
