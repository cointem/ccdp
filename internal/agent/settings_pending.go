package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
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
	target := a.sessionSettingsLocked()
	active := a.activeBinding
	sourceRevision := a.settingsRev
	a.mu.Unlock()

	if err := mutate(&target); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
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
	sandboxChanged := target.SandboxPolicy != a.cfg.SandboxMode ||
		target.AllowNetwork != a.cfg.SandboxAllowNetwork ||
		!equalStringSlice(target.AdditionalDirectories, a.cfg.AdditionalDirectories) ||
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
	a.cfg.SandboxMode = target.SandboxPolicy
	a.cfg.SandboxAllowNetwork = target.AllowNetwork
	a.cfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.cfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	a.baseCfg.SandboxMode = target.SandboxPolicy
	a.baseCfg.SandboxAllowNetwork = target.AllowNetwork
	a.baseCfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.baseCfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	if sandboxChanged {
		cfgCopy := cloneConfig(a.cfg)
		a.sandbox = buildSandbox(&cfgCopy, a.cfg.Workspace)
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
		a.emitStatus("sandbox mode → %s", target.SandboxPolicy)
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
