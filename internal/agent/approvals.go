package agent

import (
	"context"

	"ccdp/internal/access"
	"ccdp/internal/codemode"
	"ccdp/internal/jsonvalue"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
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
	answer := a.requestApprovalDetailed(tc, reason, nil, false)
	return answer.approve, answer.remember
}

func (a *Agent) requestApprovalDetailed(tc messages.ToolCall, reason string, capabilities []protocol.CapabilityRequest, resourcesOnly bool, contexts ...context.Context) approvalAnswer {
	tc.Arguments = jsonvalue.Clone(tc.Arguments)
	ctx := a.decisionContext()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	if child := a.children.childState; child != nil && child.nonInteractive {
		if child.access != nil && resourcesOnly && len(capabilities) > 0 {
			return child.access.decide(ctx, tc, reason, capabilities)
		}
		return approvalAnswer{}
	}
	defer codemode.Pause(ctx)()
	key := a.resourceDecisionKey(tc, capabilities, resourcesOnly)
	return a.awaitDecision(&decisionRequest{kind: decisionTool, ctx: ctx, call: tc, reason: reason, capabilities: append([]protocol.CapabilityRequest(nil), capabilities...), accessKey: key}).approval
}

func (a *Agent) resourceDecisionKey(tc messages.ToolCall, capabilities []protocol.CapabilityRequest, resourcesOnly bool) string {
	operation := permissions.SessionKey(tc.Name, tc.Arguments)
	if resourcesOnly {
		operation = ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return access.Key(a.conversation.sessionID, a.capabilities.revocationRevision, capabilities, operation)
}
