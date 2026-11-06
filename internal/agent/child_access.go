package agent

import (
	"context"

	"ccdp/internal/codemode"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// Only task children receive this narrow decision channel. It neither exposes
// the parent's process handles nor changes the child's frozen tool/role gates.
// Parent policy withdrawal invalidates the channel and joins all children.
type childAccessCoordinator struct {
	parent                              *Agent
	sandboxRevision, revocationRevision uint64
}

func (c *childAccessCoordinator) decide(ctx context.Context, call messages.ToolCall, reason string, requests []protocol.CapabilityRequest) approvalAnswer {
	defer codemode.Pause(ctx)()
	a := c.parent
	a.mu.Lock()
	valid := !a.capabilities.capabilityRevoking && !a.lifecycle.closing && !a.lifecycle.closed && a.services.sandbox != nil && a.services.sandbox.Revision == c.sandboxRevision && a.capabilities.revocationRevision == c.revocationRevision
	var policy = a.services.sandbox.Snapshot()
	version := capabilityPolicyVersion{sandboxRevision: c.sandboxRevision, revocationRevision: c.revocationRevision, capabilityRevision: a.capabilities.capabilityRevision}
	for _, grant := range a.capabilities.capabilityGrants {
		if applyCapability(policy, grant) != nil {
			valid = false
		}
	}
	a.mu.Unlock()
	if !valid || ctx.Err() != nil {
		return approvalAnswer{}
	}
	if denied, _ := a.services.perms.HardDeny(call.Name, call.Arguments); denied {
		return approvalAnswer{}
	}
	allGranted := true
	canonical := make([]protocol.CapabilityRequest, len(requests))
	for i, request := range requests {
		var err error
		canonical[i], err = canonicalizeCapability(policy, request)
		if err != nil {
			return approvalAnswer{}
		}
		allGranted = allGranted && capabilityGranted(policy, canonical[i])
	}
	if allGranted {
		return approvalAnswer{approve: true, capabilityScope: protocol.CapabilityScopeSession}
	}
	// Use the parent's resource key and decision queue, so root/sibling calls
	// waiting for this same access share one session grant or denial.
	answer := a.requestApprovalDetailed(call, reason, canonical, true, ctx)
	if !answer.approve {
		return answer
	}
	if answer.capabilityScope == protocol.CapabilityScopeSession {
		if _, _, err := a.addSessionCapabilities(version, canonical); err != nil {
			return approvalAnswer{}
		}
	} else {
		a.mu.Lock()
		valid = !a.capabilities.capabilityRevoking && a.services.sandbox.Revision == c.sandboxRevision && a.capabilities.revocationRevision == c.revocationRevision
		a.mu.Unlock()
		if !valid {
			return approvalAnswer{}
		}
	}
	return answer
}
