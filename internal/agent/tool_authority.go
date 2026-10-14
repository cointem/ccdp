package agent

import (
	"context"
	"errors"

	"ccdp/internal/hooks"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
)

// Each tool worker owns this adapter's invocation-local policy and approval
// version. It accesses session state through the existing admission methods.
type toolAuthority struct {
	agent        *Agent
	call         messages.ToolCall
	journal      toolJournalContext
	execution    *toolJournalExecution
	tool         tools.Tool
	policy       *sandbox.Sandbox
	version      capabilityPolicyVersion
	capabilities []protocol.CapabilityRequest
}

func (c *toolAuthority) Admit() (tooling.Admission, *tooling.Result) {
	a, tc := c.agent, c.call
	tool, out, allow, denied := a.hardAdmitTool(tc, c.journal, c.execution)
	fail := func(out string) (tooling.Admission, *tooling.Result) {
		return tooling.Admission{}, &tooling.Result{Output: out, IsError: true}
	}
	if denied {
		return fail(out)
	}
	c.tool = tool
	if _, observing := tool.(*tools.GetGoalTool); !observing && !a.goalWorkAllowed() {
		c.execution.setStatus("cancelled")
		a.emit(toolEvent(tc, "cancelled", "goal is stopped"))
		return fail("goal is stopped; no new tool work was executed")
	}
	var err error
	c.policy, c.capabilities, c.version, err = a.toolCapabilityPolicy(tc, tool)
	if err != nil {
		a.emit(toolEvent(tc, "denied", err.Error()))
		return fail("permission denied: " + err.Error())
	}
	return tooling.Admission{ConfiguredAllow: allow, NeedsCapabilities: len(c.capabilities) > 0}, nil
}
func (c *toolAuthority) GoPreHook() (plugin.ToolDecision, string) {
	return c.agent.services.gohooks.RunPreTool(c.call.Name, c.call.Arguments)
}
func (c *toolAuthority) ShellPreHook() hooks.Output {
	return c.agent.runHookWithJournal(c.agent.turn.turnCtx, hooks.EventPreToolUse, func(ctx context.Context) hooks.Output {
		return c.agent.services.hooks.PreToolUse(ctx, c.call.Name, c.call.Arguments)
	})
}
func (c *toolAuthority) CheckPermission() (permissions.Decision, string) {
	return c.agent.services.perms.Check(c.call.Name, c.call.Arguments, toolKind(c.tool))
}
func (c *toolAuthority) Approve(reason string) bool {
	var approved, remember bool
	c.policy, c.version, approved, remember = c.agent.approveToolWithCapabilities(c.call, reason, c.policy, c.version, c.capabilities)
	if remember {
		key := permissions.SessionKey(c.call.Name, c.call.Arguments)
		if approved {
			c.agent.services.perms.RememberAllow(key)
		} else {
			c.agent.services.perms.RememberDeny(key)
		}
	}
	return approved
}
func (c *toolAuthority) CheckReady() error {
	a := c.agent
	if err := a.toolExecutionError(); err != nil {
		return err
	}
	if a.capabilityApprovalCurrent(c.version) {
		return nil
	}
	a.mu.Lock()
	revoking := a.capabilities.capabilityRevoking
	a.mu.Unlock()
	if revoking {
		return errors.New("permission denied: sandbox capability revocation is pending; inspect /sandbox; this call was not run")
	}
	return errors.New("permission denied: sandbox policy changed; retry the tool under the current policy")
}
func (c *toolAuthority) Invoke() tooling.Result {
	out, failed := c.agent.runToolWithJournalPolicy(c.call, c.tool, c.journal, c.execution, c.policy)
	return tooling.Result{Output: out, IsError: failed}
}
func (c *toolAuthority) Rejected(status, reason string) {
	c.agent.emit(toolEvent(c.call, status, reason))
}

var _ tooling.Authority = (*toolAuthority)(nil)
