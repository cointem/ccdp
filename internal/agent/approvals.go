package agent

import (
	"ccdp/internal/jsonvalue"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// requestApproval blocks until the user answers the approval request. It is
// safe for concurrent tool workers: the owner queues requests and publishes
// one modal at a time.
//
// Within a turn the decision is memoized by permission SessionKey (Codex's
// approval cache): parallel workers submitting identical calls — and the
// Go-hook / shell-hook / permission gates asking about the same call — replay
// the first answer instead of re-prompting. The cache lives only for the
// current turn; lasting grants go through the remember flag / permission rules.
func (a *Agent) requestApproval(tc messages.ToolCall, reason string) (bool, bool) {
	answer := a.requestApprovalDetailed(tc, reason, nil)
	return answer.approve, answer.remember
}

func (a *Agent) requestApprovalDetailed(tc messages.ToolCall, reason string, capabilities []protocol.CapabilityRequest) approvalAnswer {
	if a.children.childState != nil && a.children.childState.nonInteractive {
		return approvalAnswer{}
	}
	tc.Arguments = jsonvalue.Clone(tc.Arguments)
	return a.awaitDecision(&decisionRequest{kind: decisionTool, ctx: a.decisionContext(), call: tc, reason: reason, capabilities: append([]protocol.CapabilityRequest(nil), capabilities...)}).approval
}

// inPlanMode reports whether plan mode is active (guarded).
func (a *Agent) inPlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.plan.planMode
}

// setPlanMode is the internal mid-turn facade for the same execution-mode
// transition used by external commands. It intentionally does not bypass the
// unified plan/permission bookkeeping.
func (a *Agent) setPlanMode(on bool) error {
	return a.setExecutionMode(on)
}

// requestPlanApproval surfaces the proposed plan and blocks until the user
// approves or rejects it. Returns true to execute.
func (a *Agent) requestPlanApproval(plan string) bool {
	if a.children.childState != nil && a.children.childState.nonInteractive {
		return false
	}
	request := &PlanRequest{ID: nextRuntimeID("plan"), Plan: plan}
	return a.awaitDecision(&decisionRequest{kind: decisionPlan, ctx: a.decisionContext(), plan: request}).plan
}
