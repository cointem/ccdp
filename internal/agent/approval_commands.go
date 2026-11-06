package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (a *Agent) applyApproveTool(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	resp := a.decisions.approvalResp
	pending := a.decisions.pendingApproval
	if resp == nil || pending == nil || pending.ID != cmd.Approval.ApprovalID {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "approval is no longer pending")
	}
	if a.decisions.approvalResolving {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "approval response was already delivered")
	}
	capabilities := append([]protocol.CapabilityRequest(nil), pending.Capabilities...)
	if len(capabilities) > 0 && cmd.Approval.Approve && cmd.Approval.CapabilityScope != protocol.CapabilityScopeOnce && cmd.Approval.CapabilityScope != protocol.CapabilityScopeSession {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "capability approval requires once or session scope")
	}
	if len(capabilities) == 0 && cmd.Approval.CapabilityScope != "" {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "approval has no capability request")
	}
	a.decisions.approvalResolving = true
	decision := "deny"
	if cmd.Approval.Approve {
		decision = "allow"
	}
	approvalID := a.decisions.approvalJournalID
	if approvalID == "" {
		approvalID = cmd.Approval.ApprovalID
	}
	reason := pending.Reason
	a.mu.Unlock()
	if err := a.persistApprovalResolved(session.ApprovalResolution{
		ApprovalID:      approvalID,
		Decision:        decision,
		Reason:          reason,
		ResolvedBy:      "user",
		CapabilityScope: cmd.Approval.CapabilityScope,
		Capabilities:    capabilities,
	}); err != nil {
		// Wake the waiting tool without asking it to write a second resolution.
		// The persistence gate remains failed, so the tool cannot cross its
		// side-effect boundary.
		select {
		case resp <- approvalAnswer{persisted: true}:
		default:
		}
		a.finishPendingDecision(decisionTool, decisionResult{err: err})
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	select {
	case resp <- approvalAnswer{approve: cmd.Approval.Approve, remember: cmd.Approval.Remember, capabilityScope: cmd.Approval.CapabilityScope, persisted: true}:
		a.finishPendingDecision(decisionTool, decisionResult{approval: approvalAnswer{approve: cmd.Approval.Approve, remember: cmd.Approval.Remember, capabilityScope: cmd.Approval.CapabilityScope, persisted: true}})
		a.publishState()
		return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
	default:
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "approval response was already delivered")
	}
}
