package agent

import (
	"context"
	"errors"
	"strings"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type decisionKind uint8

const (
	decisionTool decisionKind = iota
	decisionPlan
	decisionQuestion
)

// A decision owns one immutable request and one buffered response. The session
// loop owns the queue and visible modal; workers never hold a lock while waiting
// for a person. Cancellation removes even a request still waiting in the queue.
type decisionRequest struct {
	kind         decisionKind
	ctx          context.Context
	call         messages.ToolCall
	reason       string
	capabilities []protocol.CapabilityRequest
	plan         *PlanRequest
	question     *protocol.QuestionRequest
	result       chan decisionResult
}

func (*decisionRequest) ownerControlMessage() {}

type decisionResult struct {
	approval approvalAnswer
	plan     bool
	answer   protocol.AnswerQuestion
	err      error
}
type decisionCancelled struct{ request *decisionRequest }

func (decisionCancelled) ownerControlMessage() {}

func (a *Agent) decisionContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turn.turnCtx != nil {
		return a.turn.turnCtx
	}
	return a.lifecycle.rootCtx
}

func (a *Agent) awaitDecision(request *decisionRequest) decisionResult {
	if request.ctx == nil {
		request.ctx = context.Background()
	}
	request.result = make(chan decisionResult, 1)
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-request.ctx.Done():
		return decisionResult{err: request.ctx.Err()}
	case <-a.owner.done:
		return decisionResult{err: session.ErrClosed}
	}
	select {
	case result := <-request.result:
		return result
	case <-a.owner.done:
		return decisionResult{err: session.ErrClosed}
	case <-request.ctx.Done():
		// Cancellation cannot abandon a published approval or its durable outcome.
		select {
		case a.owner.control <- decisionCancelled{request}:
		case <-a.owner.done:
			return decisionResult{err: session.ErrClosed}
		}
		select {
		case result := <-request.result:
			return result
		case <-a.owner.done:
			return decisionResult{err: session.ErrClosed}
		}
	}
}

func (a *Agent) enqueueDecision(request *decisionRequest) {
	if len(a.owner.decisions) >= 128 {
		request.result <- decisionResult{err: errors.New("too many pending interactive requests")}
		return
	}
	a.owner.decisions = append(a.owner.decisions, request)
	a.activateDecision()
}

func (a *Agent) activateDecision() {
	for a.owner.activeDecision == nil && len(a.owner.decisions) > 0 {
		request := a.owner.decisions[0]
		a.owner.decisions[0] = nil
		a.owner.decisions = a.owner.decisions[1:]
		if err := request.ctx.Err(); err != nil {
			request.result <- decisionResult{err: err}
			continue
		}
		if a.persistenceFailure() != nil {
			request.result <- decisionResult{err: a.persistenceFailure()}
			continue
		}
		switch request.kind {
		case decisionTool:
			key := permissions.SessionKey(request.call.Name, request.call.Arguments)
			a.mu.Lock()
			approved, cached := a.decisions.approvalCache[key]
			a.mu.Unlock()
			if cached && len(request.capabilities) == 0 {
				request.result <- decisionResult{approval: approvalAnswer{approve: approved}}
				continue
			}
			fact := toolApprovalRequest(a, request.call, request.reason, request.capabilities)
			if err := a.persistApprovalRequested(fact); err != nil {
				request.result <- decisionResult{err: err}
				continue
			}
			reason := request.reason
			if len(request.capabilities) > 0 {
				reason = strings.TrimSpace(reason + "\n" + formatCapabilityRequests(request.capabilities))
			}
			pending := &ApprovalRequest{ID: request.call.ID, Tool: request.call.Name, Command: prettyArgs(request.call.Arguments), Reason: reason, Capabilities: append([]protocol.CapabilityRequest(nil), request.capabilities...), journalID: fact.ApprovalID}
			a.mu.Lock()
			a.decisions.pendingApproval = pending
			a.decisions.approvalResp = make(chan approvalAnswer, 1)
			a.decisions.approvalResolving = false
			a.mu.Unlock()
			a.owner.activeDecision = request
			a.emit(Event{Type: EventApproval, Approval: pending})
		case decisionPlan:
			fact := session.WorkflowState{Phase: string(protocol.WorkflowAwaitingDecision), PlanID: request.plan.ID, PlanVersion: planViewVersion}
			if err := a.persistWorkflowFact(fact); err != nil {
				request.result <- decisionResult{err: err}
				continue
			}
			a.mu.Lock()
			a.plan.pendingPlan = request.plan
			a.plan.planResp = make(chan bool, 1)
			a.lifecycle.workflow = protocol.WorkflowAwaitingDecision
			a.mu.Unlock()
			a.owner.activeDecision = request
			a.publishState()
			a.emit(Event{Type: EventPlan, Plan: request.plan})
			a.emitStatus("plan ready — approve (y) to execute, deny (n) to reject")
		case decisionQuestion:
			a.mu.Lock()
			a.decisions.pendingQuestion = request.question
			a.decisions.questionResp = make(chan protocol.AnswerQuestion, 1)
			a.mu.Unlock()
			a.owner.activeDecision = request
			a.publishState()
			a.emit(Event{Type: EventQuestion, Question: protocol.CloneQuestionRequest(request.question)})
		}
	}
}

// Called after the command's durable resolution, before another modal is made
// visible. A queued identical tool call sees the cache from this resolution.
func (a *Agent) finishPendingDecision(kind decisionKind, result decisionResult) {
	request := a.owner.activeDecision
	if request == nil || request.kind != kind {
		return
	}
	a.mu.Lock()
	switch kind {
	case decisionTool:
		if result.err == nil && len(request.capabilities) == 0 {
			if a.decisions.approvalCache == nil {
				a.decisions.approvalCache = make(map[string]bool)
			}
			a.decisions.approvalCache[permissions.SessionKey(request.call.Name, request.call.Arguments)] = result.approval.approve
		}
		a.decisions.pendingApproval, a.decisions.approvalResp, a.decisions.approvalResolving = nil, nil, false
	case decisionPlan:
		a.plan.pendingPlan, a.plan.planResp = nil, nil
	case decisionQuestion:
		a.decisions.pendingQuestion, a.decisions.questionResp = nil, nil
	}
	a.mu.Unlock()
	a.owner.activeDecision = nil
	a.publishState()
	a.refreshSnapshotOwned()
	request.result <- result
	a.activateDecision()
}

func (a *Agent) cancelDecision(request *decisionRequest) {
	if a.owner.activeDecision != request {
		for i, queued := range a.owner.decisions {
			if queued == request {
				copy(a.owner.decisions[i:], a.owner.decisions[i+1:])
				a.owner.decisions[len(a.owner.decisions)-1] = nil
				a.owner.decisions = a.owner.decisions[:len(a.owner.decisions)-1]
				request.result <- decisionResult{err: request.ctx.Err()}
				return
			}
		}
		return // An answer already won; its buffered result remains authoritative.
	}
	result := decisionResult{err: request.ctx.Err()}
	switch request.kind {
	case decisionTool:
		a.mu.Lock()
		id := a.decisions.pendingApproval.journalID
		a.mu.Unlock()
		if err := a.persistApprovalResolved(session.ApprovalResolution{ApprovalID: id, Decision: "cancel", Reason: "interrupted", ResolvedBy: "runtime"}); err != nil {
			result.err = err
		}
		a.emitStatus("approval skipped (interrupted)")
	case decisionPlan:
		a.mu.Lock()
		drafting := a.plan.planMode
		a.mu.Unlock()
		if drafting {
			err := a.persistWorkflowFact(session.WorkflowState{Phase: string(protocol.WorkflowDrafting), PlanID: request.plan.ID, PlanVersion: planViewVersion})
			if err != nil {
				result.err = err
			} else {
				a.mu.Lock()
				a.lifecycle.workflow = protocol.WorkflowDrafting
				a.mu.Unlock()
			}
		}
		a.emitStatus("plan approval skipped (interrupted)")
	}
	a.finishPendingDecision(request.kind, result)
}
