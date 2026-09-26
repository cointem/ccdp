package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/mcp"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

// This file owns the single admission path for every runtime-mutable setting:
// permission policy, sandbox policy, execution (plan) mode, model and reasoning
// effort/verbosity. Every change applies to live runtime state immediately, idle
// or mid-turn. This is safe because each provider request freezes its own config
// and binding at the step boundary (captureStepRuntime) and each authorization
// gate reads the mode atomically, so writing live state can never disturb the
// in-flight request or a decision already made — it only governs the next one.
// This mirrors how the sibling CLIs flip permission mode or model mid-run.

// settingsMutator applies one command's delta to a settings target. It returns an
// error to reject the command before anything is persisted.
type settingsMutator func(target *session.Settings) error

// applySettingsCommand is the single admission path for every mutable setting.
// Every change applies immediately to live runtime state, idle or mid-turn: each
// provider request freezes its own config and binding at the step boundary
// (captureStepRuntime) and each authorization gate reads the mode atomically, so
// writing live state can never disturb the in-flight request or a decision already
// made — it only governs the next one. The caller must not hold a.mu or
// settingsCommitMu.
func (a *Agent) applySettingsCommand(cmd protocol.Command, mutate settingsMutator) protocol.Receipt {
	// Serialize capture/commit/publish so a step boundary cannot interleave with a
	// concurrent settings command. This may cover durable I/O; ordinary readers use
	// the read side of the same lock.
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()

	a.mu.Lock()
	if a.capabilityRevoking {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "sandbox capability revocation is in progress")
	}
	target := a.sessionSettingsLocked()
	active := a.activeBinding
	sourceRevision := a.settingsRev
	oldSandbox := a.sandbox
	oldConfig := cloneConfig(a.cfg)
	a.mu.Unlock()

	if err := mutate(&target); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}

	// A sandbox snapshot is inherited by every process and remote connection.
	// Before a tightening change, close admission and join old execution paths;
	// widening changes can safely govern future calls without invalidating older,
	// more restrictive children.
	a.mu.Lock()
	sandboxChanged := target.NetworkAccess != a.cfg.NetworkAccess ||
		!equalStringSlice(target.AdditionalDirectories, a.cfg.AdditionalDirectories) ||
		!equalStringSlice(target.AdditionalReadOnlyDirectories, a.cfg.AdditionalReadOnlyDirectories) ||
		!equalStringSlice(target.DisallowedDirectories, a.cfg.DisallowedDirectories)
	a.mu.Unlock()
	sandboxTightened := false
	if sandboxChanged && oldSandbox != nil {
		nextConfig := cloneConfig(&oldConfig)
		nextConfig.NetworkAccess = target.NetworkAccess
		nextConfig.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
		nextConfig.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
		nextConfig.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
		nextSandbox := buildSandbox(&nextConfig, nextConfig.Workspace)
		sandboxTightened = sandbox.PolicyTightened(oldSandbox, nextSandbox)
	}
	quiesced := false
	committed := false
	defer func() {
		if quiesced && !committed {
			a.finishSandboxSettingsChange(false)
		}
	}()
	if sandboxTightened {
		if err := a.quiesceForSandboxSettingsChange(nil, true); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, "sandbox policy change is waiting for old executions to stop; dispatch remains blocked: "+err.Error())
		}
		quiesced = true
	}

	// Resolve the model client only when the model dimension actually moves.
	var binding *modelBinding
	if target.Model != active.model || target.Provider != active.provider || target.Endpoint != active.endpoint {
		resolved, err := a.makeBinding(target.Model)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
		binding = &resolved
		target.Model = resolved.model
		target.Provider = resolved.provider
		target.Endpoint, _ = sanitizeRequestEndpoint(resolved.endpoint)
	}

	nextRevision := sourceRevision + 1
	if nextRevision == 0 {
		nextRevision = 1
	}

	if err := a.persistSettingsFact(target, nextRevision); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.applySettingsSnapshot(target, binding, nextRevision)
	if sandboxTightened {
		a.finishSandboxSettingsChange(true)
		committed = true
	} else if sandboxChanged {
		a.mu.Lock()
		manager, policy := a.mcp, a.sandbox
		a.mu.Unlock()
		if manager != nil && policy != nil {
			manager.SetSandbox(policy.Snapshot())
		}
	}
	if cmd.Type == protocol.CommandSetPermissionPolicy && a.childState == nil {
		a.mu.Lock()
		cfg := cloneConfig(a.cfg)
		a.mu.Unlock()
		if err := cfg.SaveProjectPermission(target.PermissionPolicy); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, "session mode changed, but saving project permission failed: "+err.Error())
		}
	}
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

func sandboxQuiescenceOwner(name string) bool {
	switch name {
	case "reload", "cd", "trust":
		return true
	default:
		return false
	}
}

// quiesceForSandboxSettingsChange is called while settingsCommitMu is held.
// It freezes publication/admission, then releases that lock while it joins old
// work. ownerCtx identifies an asynchronous reload/cd/trust operation that is
// itself performing the transaction; it must not cancel or wait on itself.
// closeMCP is false for candidate publication, which refreshes MCP atomically
// after the old in-flight turn has drained.
func (a *Agent) quiesceForSandboxSettingsChange(ownerCtx context.Context, closeMCP bool) error {
	a.mu.Lock()
	if a.capabilityRevoking {
		a.mu.Unlock()
		return errors.New("sandbox capability revocation is already pending")
	}
	a.capabilityRevoking = true
	a.capabilityRevision++
	ownerOperation := ownerCtx != nil && a.turnCtx == ownerCtx
	turnCancel, compactCancel := a.turnCancel, a.compactCancel
	if ownerOperation {
		turnCancel = nil
	}
	resources, oldMCP, supervisor := a.resources, a.mcp, a.supervisor
	activeSandbox := a.sandbox
	a.mu.Unlock()

	// beginStepChecked and supervisor.prepare also need this publication lock.
	// With admission frozen, releasing it lets already-started callers observe
	// the gate and return before this function joins their owning operations.
	a.settingsCommitMu.Unlock()
	defer a.settingsCommitMu.Lock()

	var stopErr error
	if resources != nil && resources.Processes != nil {
		if err := resources.Processes.StopAll(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("stop background processes: %w", err))
		}
	}
	if turnCancel != nil {
		turnCancel()
	}
	if compactCancel != nil {
		compactCancel()
	}
	if closeMCP && oldMCP != nil {
		if err := oldMCP.Close(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("close MCP connections: %w", err))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if supervisor != nil {
		if err := supervisor.revokeCapabilities(ctx); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("stop descendant sessions: %w", err))
		}
	}
	if err := waitGroupContext(ctx, &a.turnWG); err != nil {
		stopErr = errors.Join(stopErr, fmt.Errorf("wait for active turn: %w", err))
	}
	if err := waitGroupContext(ctx, &a.sandboxOperationWG); err != nil {
		stopErr = errors.Join(stopErr, fmt.Errorf("wait for active runtime operations: %w", err))
	}
	if activeSandbox != nil && activeSandbox.ExternalExecutionPossible() {
		stopErr = errors.Join(stopErr, errors.New("a local process may have descendants retaining the previous Seatbelt policy; restart the session and independently stop possible descendants before retrying the change"))
	}
	return stopErr
}

// finishSandboxSettingsChange makes the next execution use a fresh manager
// bound to the installed snapshot. On a persistence failure, it restores the
// previous snapshot and grants; on success, grants are cleared because the
// user-visible authorization boundary changed.
func (a *Agent) finishSandboxSettingsChange(clearGrants bool) {
	a.mu.Lock()
	if a.sandbox == nil {
		a.capabilityRevoking = true
		a.mu.Unlock()
		return
	}
	replacement := mcp.NewManager()
	replacement.SetSandbox(a.sandbox.Snapshot())
	replacement.RegisterTools(a.registry)
	a.mcp = replacement
	for name := range a.deferTools {
		if len(name) >= 5 && name[:5] == "mcp__" {
			delete(a.deferTools, name)
			delete(a.discovered, name)
		}
	}
	if clearGrants {
		clear(a.capabilityGrants)
		a.capabilityRevision++
	}
	resources := a.resources
	a.capabilityRevoking = false
	a.mu.Unlock()
	if resources != nil && resources.Processes != nil {
		resources.Processes.ResumeAfterRevocation()
	}
}

// applySettingsSnapshot mutates live runtime state to match a target snapshot. It
// is the single application point for every settings command, so a setting
// behaves identically however it was admitted. Side effects that must run outside
// a.mu (client routing, permission manager, hook context, events) follow the
// critical section.
func (a *Agent) applySettingsSnapshot(target session.Settings, binding *modelBinding, revision uint64) {
	a.mu.Lock()
	modelChanged := binding != nil
	permModeChanged := target.PermissionPolicy != a.cfg.PermissionMode
	allowChanged := !equalStringSlice(target.AlwaysAllow, a.cfg.AlwaysAllow)
	denyChanged := !equalStringSlice(target.AlwaysDeny, a.cfg.AlwaysDeny)
	sandboxChanged := target.NetworkAccess != a.cfg.NetworkAccess ||
		!equalStringSlice(target.AdditionalDirectories, a.cfg.AdditionalDirectories) ||
		!equalStringSlice(target.AdditionalReadOnlyDirectories, a.cfg.AdditionalReadOnlyDirectories) ||
		!equalStringSlice(target.DisallowedDirectories, a.cfg.DisallowedDirectories)
	wantPlan := target.ExecutionMode == "plan"
	planChanged := wantPlan != a.planMode
	permChanged := permModeChanged || allowChanged || denyChanged

	mode, modeErr := permissions.ParseMode(target.PermissionPolicy)
	if modeErr != nil || mode == permissions.ModePlan {
		// The snapshot's PermissionPolicy is always a concrete base mode; fall back
		// to default if it is empty or unexpectedly plan-valued.
		mode = permissions.ModeDefault
	}

	// generation
	a.cfg.ReasoningEffort, a.baseCfg.ReasoningEffort = target.ReasoningEffort, target.ReasoningEffort
	a.cfg.Verbosity, a.baseCfg.Verbosity = target.Verbosity, target.Verbosity

	// permission
	a.cfg.PermissionMode = string(mode)
	a.baseCfg.PermissionMode = string(mode)
	a.cfg.AlwaysAllow = cloneStringSlice(target.AlwaysAllow)
	a.cfg.AlwaysDeny = cloneStringSlice(target.AlwaysDeny)
	a.baseCfg.AlwaysAllow = cloneStringSlice(target.AlwaysAllow)
	a.baseCfg.AlwaysDeny = cloneStringSlice(target.AlwaysDeny)

	// execution mode / plan
	a.planMode = wantPlan
	a.planBaseMode = mode
	if planChanged {
		if wantPlan {
			a.workflow = protocol.WorkflowDrafting
		} else {
			a.workflow = protocol.WorkflowOff
		}
	}

	// sandbox
	a.cfg.NetworkAccess = target.NetworkAccess
	a.cfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.cfg.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
	a.cfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	a.baseCfg.NetworkAccess = target.NetworkAccess
	a.baseCfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.baseCfg.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
	a.baseCfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	if sandboxChanged {
		cfgCopy := cloneConfig(a.cfg)
		newSandbox := buildSandbox(&cfgCopy, a.cfg.Workspace)
		newSandbox.ShareExecutionGuard(a.sandbox)
		a.sandbox = newSandbox
		a.sandbox.Revision = revision
		// This is the monotonic sandbox-approval epoch. Do not rely only on the
		// backend snapshot Revision, which may start above settingsRev and later
		// be replaced by it.
		a.capabilityRevision++
	}

	// model
	if binding != nil {
		a.applyBindingLocked(*binding)
	}

	a.settingsRev = revision
	routeCfg := cloneConfig(a.cfg)
	a.mu.Unlock()

	if binding != nil {
		a.registerBindingRoutes(*binding, routeCfg)
	}
	if permChanged || planChanged {
		a.perms.SetMode(mode)
	}
	if permChanged {
		a.perms.SetPolicy(permissions.Policy{
			AlwaysAllow: cloneStringSlice(target.AlwaysAllow),
			AlwaysDeny:  cloneStringSlice(target.AlwaysDeny),
		})
	}
	if permChanged || sandboxChanged || planChanged {
		a.syncHookContext()
	}
	if modelChanged {
		a.emit(Event{Type: EventModeChanged, Text: "model:" + binding.model})
		a.emitStatus("model switched to %s", binding.model)
	}
	if planChanged {
		a.emit(Event{Type: EventPlanModeChanged, PlanMode: wantPlan})
		a.emitStatus("plan mode %s", map[bool]string{true: "on", false: "off"}[wantPlan])
	} else if permChanged {
		a.emit(Event{Type: EventModeChanged, Mode: mode})
		a.emitStatus("permission policy updated")
	}
	if sandboxChanged {
		a.emit(Event{Type: EventSandboxChanged})
		a.emitStatus("sandbox policy updated (network: %v, read/write roots: %d, read-only roots: %d)",
			target.NetworkAccess, len(target.AdditionalDirectories), len(target.AdditionalReadOnlyDirectories))
	}
	a.publishState()
}

func cloneStringSlice(src []string) []string {
	if src == nil {
		return nil
	}
	return append([]string(nil), src...)
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
