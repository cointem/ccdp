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
	if a.capabilities.capabilityRevoking {
		return fmt.Errorf("sandbox capability revocation is pending")
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
	if a.inPlanMode() && !planAllowedImplementation(tool) {
		return fmt.Errorf("blocked in plan mode: %s modifies state", tc.Name)
	}
	if denied, reason := a.services.perms.HardDeny(tc.Name, tc.Arguments); denied {
		return fmt.Errorf("permission denied: %s", reason)
	}
	if a.discovery.customPerms[tc.Name] == "deny" {
		return fmt.Errorf("permission denied: %s is denied by tool config", tc.Name)
	}
	if execution != nil {
		return execution.start(a, tc)
	}
	return nil
}
