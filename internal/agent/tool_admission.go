package agent

import (
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/tools"
)

// hardAdmitTool resolves the frozen tool implementation and applies the
// non-overridable admission gates: child/guardian hard policy, plan-mode
// mutation block, explicit hard denies, strict-sandbox network blocks, and
// config-level custom-tool permissions. These must run before any hook,
// permission override, or Tool.Run; a name a custom/plugin scope can shadow is
// never used to classify capability. configuredAllow reports the custom "allow" setting; it does not bypass hooks.
func (a *Agent) hardAdmitTool(tc messages.ToolCall, journal toolJournalContext, execution *toolJournalExecution) (tool tools.Tool, output string, configuredAllow, denied bool) {
	var ok bool
	if journal.enabled && journal.toolLease != nil {
		tool, ok = journal.toolLease.Get(tc.Name)
	} else {
		tool, ok = a.services.registry.Get(tc.Name)
	}
	if denied, reason := ChildToolGate(a, tc, tool); denied {
		a.emit(toolEvent(tc, "denied", reason))
		return nil, fmt.Sprintf("permission denied: %s", reason), false, true
	}
	if !ok {
		a.emit(toolEvent(tc, "error", "unknown tool "+tc.Name))
		return nil, fmt.Sprintf("Error: unknown tool %q", tc.Name), false, true
	}
	if purpose, _, _, child := ChildRuntime(a); child && (purpose == childPurposeGuardian || a.children.childState.readOnlyWorkspace) && (!readOnlyImplementation(tool) || (purpose == childPurposeGuardian && toolKind(tool) != permissions.KindRead)) && !(isTaskChild(purpose) && childMessageTool(tool)) {
		reason := fmt.Sprintf("guardian hard deny: tool %q is not the built-in read-only implementation", tc.Name)
		a.emit(toolEvent(tc, "denied", reason))
		return nil, fmt.Sprintf("permission denied: %s", reason), false, true
	}
	// Plan mode hard-blocks mutating tools even if the model somehow calls one.
	// Capability is determined from the frozen concrete implementation, never
	// from a name that a custom/plugin scope can shadow.
	if a.inPlanMode() && !planAllowedImplementation(tool) {
		a.emit(toolEvent(tc, "denied", "blocked in plan mode"))
		return nil, fmt.Sprintf("blocked in plan mode: %s modifies state; only built-in read-only tools are available until the plan is approved", tc.Name), false, true
	}
	// Explicit hard denials are checked before any custom permission, Go-hook,
	// or shell-hook allow path. Hooks can add an approval requirement, but they
	// must never turn an always-deny/dangerous invocation into an execution.
	if denied, reason := a.services.perms.HardDeny(tc.Name, tc.Arguments); denied {
		a.emit(toolEvent(tc, "denied", reason))
		return nil, fmt.Sprintf("permission denied: %s", reason), false, true
	}

	// Custom tools may carry a config-level permission override that bypasses
	// the normal approval gate ("allow" runs it, "deny" refuses it).
	a.mu.Lock()
	perm, isCustom := a.discovery.customPerms[tc.Name]
	a.mu.Unlock()
	if isCustom {
		switch perm {
		case "allow":
			return tool, "", true, false
		case "deny":
			a.emit(toolEvent(tc, "denied", "denied by tool config"))
			return nil, fmt.Sprintf("permission denied: %s is configured with permission \"deny\"", tc.Name), false, true
		}
	}
	return tool, "", false, false
}
