package agent

import (
	"context"
	"errors"

	"ccdp/internal/execution"
	"ccdp/internal/hooks"
	"ccdp/internal/jsonvalue"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
)

// Each tool worker owns this adapter's invocation-local policy and approval
// version. It accesses session state through the existing admission methods.
type toolAuthority struct {
	agent          *Agent
	call           messages.ToolCall
	journal        toolJournalContext
	execution      *toolJournalExecution
	tool           tools.Tool
	policy         *sandbox.Sandbox
	version        capabilityPolicyVersion
	capabilities   []protocol.CapabilityRequest
	hookAsk        bool
	decisionSource string
}

func (c *toolAuthority) Admit() (tooling.Admission, *tooling.Result) {
	a, tc := c.agent, c.call
	a.emitToolPhase(tc, protocol.ExecutionPreparing)
	if c.journal.context != nil && c.journal.context.Err() != nil {
		c.execution.setStatus("cancelled")
		return tooling.Admission{}, &tooling.Result{Output: c.journal.context.Err().Error(), IsError: true, Status: "cancelled"}
	}
	args, err := c.journal.toolLease.PrepareArguments(tc.Name, tc.Arguments)
	if err != nil {
		c.execution.setStatus("error")
		return tooling.Admission{}, &tooling.Result{Output: "invalid tool arguments/schema: " + err.Error(), IsError: true, Status: "error"}
	}
	tc.Arguments = args
	c.call = tc
	tool, out, allow, denied := a.hardAdmitTool(tc, c.journal)
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
	c.policy, c.capabilities, c.version, err = a.toolCapabilityPolicy(tc, tool)
	if err != nil {
		a.emit(toolEvent(tc, "denied", err.Error()))
		if execution.CheckEnvironment(a.currentSandbox()) != nil {
			c.execution.failure = &protocol.ToolFailure{Code: "environment_unavailable", DecisionSource: "environment", ExecutionStarted: "no", RetryClass: "after_environment_change"}
		}
		return fail("permission denied: " + err.Error())
	}
	return tooling.Admission{ConfiguredAllow: allow, NeedsCapabilities: len(c.capabilities) > 0}, nil
}
func (c *toolAuthority) GoPreHook() (plugin.ToolDecision, string) {
	decision, reason := c.agent.services.gohooks.RunPreTool(c.call.Name, c.call.Arguments)
	c.hookAsk = c.hookAsk || decision == plugin.DecisionAsk
	if decision == plugin.DecisionDeny {
		c.decisionSource = "hook"
	}
	return decision, reason
}
func (c *toolAuthority) ShellPreHook() hooks.Output {
	ctx := c.journal.context
	if ctx == nil {
		ctx = c.agent.turn.turnCtx
	}
	out := c.agent.runHookWithJournal(ctx, hooks.EventPreToolUse, func(ctx context.Context) hooks.Output {
		return c.agent.services.hooks.PreToolUse(ctx, c.call.Name, c.call.Arguments)
	})
	c.hookAsk = c.hookAsk || out.Decision == hooks.DecisionAsk
	if out.Decision == hooks.DecisionDeny || out.Decision == hooks.DecisionBlock {
		c.decisionSource = "hook"
	}
	return out
}
func (c *toolAuthority) CheckPermission() (permissions.Decision, string) {
	return c.agent.services.perms.Check(c.call.Name, c.call.Arguments, invocationKind(c.tool, c.call.Arguments))
}
func (c *toolAuthority) Approve(reason string, resourcesOnly bool) bool {
	c.agent.emitToolPhase(c.call, protocol.ExecutionWaitingAccess)
	key := c.agent.resourceDecisionKey(c.call, c.capabilities, resourcesOnly)
	c.agent.mu.Lock()
	previous, cached := c.agent.decisions.resourceDecisions[key]
	c.agent.mu.Unlock()
	if cached && !previous.approve {
		c.accessDenied(key, "cached_decision")
		return false
	}
	cfg := c.agent.configSnapshot()
	if cfg.PermissionMode == string(permissions.ModeAuto) && cfg.GuardianEnabled() && !c.hookAsk && !cached {
		review := c.call
		review.Arguments = jsonvalue.Clone(c.call.Arguments)
		if review.Arguments == nil {
			review.Arguments = map[string]any{}
		}
		review.Arguments["missing_access"] = c.capabilities
		err := c.agent.guardianCheck(review, c.journal.context)
		if err == nil {
			// The reviewer can grant only this already-validated invocation.
			// Session authority always requires an explicit user decision.
			if !c.agent.capabilityApprovalCurrent(c.version) {
				return false
			}
			policy := c.policy.Snapshot()
			for _, request := range c.capabilities {
				if applyCapability(policy, request) != nil {
					return false
				}
			}
			policy.SingleUse = true
			fact := toolApprovalRequest(c.agent, c.call, "exception reviewer approved this invocation", c.capabilities)
			if c.agent.persistApprovalRequested(fact) != nil {
				return false
			}
			if c.agent.persistApprovalResolved(session.ApprovalResolution{ApprovalID: fact.ApprovalID, Decision: "allow", ResolvedBy: "reviewer", Reason: "exception review", Capabilities: c.capabilities, CapabilityScope: protocol.CapabilityScopeOnce}) != nil {
				return false
			}
			c.policy = policy
			return true
		}
		if !errors.Is(err, errGuardianUnavailable) {
			c.accessDenied(key, "reviewer")
			c.agent.emitStatus("exception review denied: %v", err)
			if key != "" && (c.journal.context == nil || c.journal.context.Err() == nil) {
				c.agent.mu.Lock()
				c.agent.cacheResourceDecisionLocked(key, approvalAnswer{})
				c.agent.mu.Unlock()
			}
			return false
		}
		reason += "; " + err.Error()
	}
	var approved, remember bool
	c.policy, c.version, approved, remember = c.agent.approveToolWithCapabilities(c.call, reason, resourcesOnly, c.policy, c.version, c.capabilities, c.journal.context)
	if !approved {
		c.accessDenied(key, "access_coordinator")
	}
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
	if c.journal.context != nil && c.journal.context.Err() != nil {
		return c.journal.context.Err()
	}
	a := c.agent
	if err := a.toolExecutionError(); err != nil {
		return err
	}
	if controlTool(c.tool, c.call.Arguments) || toolKind(c.tool) == permissions.KindComputer {
		return nil
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
	// No side effect has started. Refresh the original invocation internally,
	// including any newly required access, instead of spending a model turn.
	policy, missing, version, err := a.toolCapabilityPolicy(c.call, c.tool)
	if err != nil {
		return err
	}
	c.policy, c.capabilities, c.version = policy, missing, version
	decision, reason := c.CheckPermission()
	if decision == permissions.DecisionDeny {
		return errors.New(reason)
	}
	if decision == permissions.DecisionAsk || len(missing) != 0 {
		if !c.Approve(reason, decision != permissions.DecisionAsk) {
			return errors.New("access denied while refreshing the pending invocation")
		}
	}
	if !a.capabilityApprovalCurrent(c.version) {
		return errors.New("execution environment is changing; invocation was not started")
	}
	return nil
}
func (c *toolAuthority) Invoke() tooling.Result {
	c.agent.emitToolPhase(c.call, protocol.ExecutionAdmitted)
	c.agent.emitToolPhase(c.call, protocol.ExecutionDispatching)
	return c.agent.runToolWithJournalPolicy(c.call, c.tool, c.journal, c.execution, c.policy)
}
func (c *toolAuthority) Rejected(status, reason string) {
	if c.execution.failure == nil {
		source := c.decisionSource
		if source == "" {
			source = "policy"
		}
		c.execution.failure = &protocol.ToolFailure{Code: "policy_denied", DecisionSource: source, ExecutionStarted: "no", RetryClass: "after_policy_change"}
	}
	c.execution.setStatus(status)
	c.agent.emit(toolEvent(c.call, status, reason))
}

func (c *toolAuthority) accessDenied(key, source string) {
	c.execution.failure = &protocol.ToolFailure{Code: "access_denied", DecisionSource: source, ExecutionStarted: "no", RetryClass: "after_access_change", Resources: append([]protocol.CapabilityRequest(nil), c.capabilities...), DecisionKey: key}
}

var _ tooling.Authority = (*toolAuthority)(nil)
