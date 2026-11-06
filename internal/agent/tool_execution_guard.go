package agent

import (
	"context"
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

// All execution paths share this gate, including direct calls and approvals.
func (a *Agent) toolExecutionError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.services.persistenceErr != nil {
		return fmt.Errorf("session persistence failed: %w", a.services.persistenceErr)
	}
	if a.lifecycle.closing || a.lifecycle.closed || a.turn.interruptFlag {
		return context.Canceled
	}
	ctx := a.turn.turnCtx
	if ctx == nil {
		ctx = a.lifecycle.rootCtx
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// Approval and guardian work may have waited while settings changed. Recheck
// the current non-overridable gates against the already selected implementation
// at the common start boundary, including callers that bypass the UI pipeline.
func (a *Agent) admitToolStartOwned(tc messages.ToolCall, tool tools.Tool, execution *toolJournalExecution) error {
	a.settingsCommitMu.RLock()
	defer a.settingsCommitMu.RUnlock()
	if err := a.toolExecutionError(); err != nil {
		return err
	}
	a.mu.Lock()
	revoking := a.capabilities.capabilityRevoking
	desktopEnabled := a.services.cfg.ComputerUseEnabled && a.children.childState == nil
	a.mu.Unlock()
	_, desktop := tool.(*tools.ComputerTool)
	if desktop && !desktopEnabled {
		return fmt.Errorf("permission denied: Computer requires an explicitly enabled root session")
	}
	if revoking && !controlTool(tool, tc.Arguments) && !desktop {
		return fmt.Errorf("execution environment is quarantined; invocation was not started")
	}
	if denied, reason := a.services.perms.HardDeny(tc.Name, tc.Arguments); denied {
		return fmt.Errorf("permission denied: %s", reason)
	}
	if a.discovery.customPerms[tc.Name] == "deny" {
		return fmt.Errorf("permission denied: %s is denied by tool config", tc.Name)
	}
	if ctx := execution.ctx.context; ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return execution.start(a, tc)
}
