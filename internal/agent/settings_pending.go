package agent

import (
	"errors"

	"ccdp/internal/mcp"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

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
func (a *Agent) applySettingsCommand(cmd protocol.Command, mutate settingsMutator, owned ...bool) protocol.Receipt {
	// Serialize capture/commit/publish so a step boundary cannot interleave with a
	// concurrent settings command. This may cover durable I/O; ordinary readers use
	// the read side of the same lock.
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()

	a.mu.Lock()
	if a.capabilities.capabilityRevoking && (cmd.Type == protocol.CommandSetSandboxPolicy || cmd.Type == protocol.CommandSetWorkspace || cmd.Type == protocol.CommandTrustProject) {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "sandbox capability revocation is in progress")
	}
	target := a.sessionSettingsLocked()
	active := a.modelsState.activeBinding
	sourceRevision := a.sequences.settingsRev
	oldSandbox := a.services.sandbox
	oldConfig := cloneConfig(a.services.cfg)
	var frozenModels *plugin.ModelRegistry
	if a.services.models != nil {
		frozenModels = a.services.models.Freeze()
	}
	a.mu.Unlock()

	if err := mutate(&target); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}

	// A sandbox snapshot is inherited by every process and remote connection.
	// Before a tightening change, close admission and join old execution paths;
	// widening changes can safely govern future calls without invalidating older,
	// more restrictive children.
	a.mu.Lock()
	sandboxChanged := target.NetworkAccess != a.services.cfg.NetworkAccess || target.LocalDevelopmentNetwork != a.services.cfg.LocalDevelopmentNetwork ||
		!equalStringSlice(target.AdditionalDirectories, a.services.cfg.AdditionalDirectories) ||
		!equalStringSlice(target.AdditionalReadOnlyDirectories, a.services.cfg.AdditionalReadOnlyDirectories) ||
		!equalStringSlice(target.DisallowedDirectories, a.services.cfg.DisallowedDirectories)
	a.mu.Unlock()
	sandboxTightened := false
	if sandboxChanged && oldSandbox != nil {
		nextConfig := cloneConfig(&oldConfig)
		nextConfig.NetworkAccess = target.NetworkAccess
		nextConfig.LocalDevelopmentNetwork = target.LocalDevelopmentNetwork
		nextConfig.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
		nextConfig.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
		nextConfig.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
		nextSandbox := rebuildSandbox(&nextConfig, nextConfig.Workspace, oldSandbox)
		sandboxTightened = sandbox.PolicyTightened(oldSandbox, nextSandbox)
	}
	var scope *sandboxQuiescence
	if sandboxTightened {
		var err error
		scope, err = a.beginSandboxQuiescence(nil, true)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
		}
	}
	a.mu.Lock()
	a.capabilities.settingsPreparing = true
	a.mu.Unlock()
	// Registered before unlock: no newly admitted turn can race the drain.
	defer a.finishSettingsPreparation()
	input := settingsPreparation{target: target, config: oldConfig, models: frozenModels, active: active, sourceRevision: sourceRevision, quiescence: scope}
	a.settingsCommitMu.Unlock()
	var prepared settingsPrepared
	if len(owned) > 0 && owned[0] {
		done := make(chan settingsPrepared, 1)
		go func() { done <- prepareSettings(input) }()
		prepared = a.awaitSettings(done)
	} else {
		prepared = prepareSettings(input)
	}
	a.settingsCommitMu.Lock()
	committed := false
	defer func() {
		if prepared.quiesced && !committed {
			a.finishSandboxSettingsChange(false)
		}
	}()
	if prepared.err != nil {
		code := protocol.ErrorInvalidCommand
		if scope != nil {
			code = protocol.ErrorInternal
		}
		return a.rejectedReceipt(cmd, code, prepared.err.Error())
	}
	a.mu.Lock()
	stale := a.sequences.settingsRev != prepared.sourceRevision
	closing := a.lifecycle.closing || a.lifecycle.closed
	a.mu.Unlock()
	if closing {
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session closed during settings preparation")
	}
	if stale {
		return a.rejectedReceipt(cmd, protocol.ErrorStaleRevision, "settings changed during preparation")
	}
	target, binding := prepared.target, prepared.binding

	nextRevision := sourceRevision + 1
	if nextRevision == 0 {
		nextRevision = 1
	}

	if err := a.persistSettingsFact(target, nextRevision); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.applySettingsSnapshot(target, binding, nextRevision)
	if sandboxTightened {
		committed = true
		if err := a.finishSandboxSettingsChange(true); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, "settings saved; execution remains paused: "+err.Error())
		}
	} else if sandboxChanged {
		a.mu.Lock()
		manager, policy := a.services.mcp, a.services.sandbox
		a.mu.Unlock()
		if manager != nil && policy != nil {
			manager.SetSandbox(policy.Snapshot())
		}
	}
	if cmd.Type == protocol.CommandSetPermissionPolicy && a.children.childState == nil {
		a.mu.Lock()
		cfg := cloneConfig(a.services.cfg)
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

// finishSandboxSettingsChange makes the next execution use a fresh manager
// bound to the installed snapshot. On a persistence failure, it restores the
// previous snapshot and grants; on success, grants are cleared because the
// user-visible authorization boundary changed.
func (a *Agent) finishSandboxSettingsChange(clearGrants bool) error {
	a.mu.Lock()
	if a.services.sandbox == nil {
		a.capabilities.capabilityRevoking = true
		a.mu.Unlock()
		return errors.New("sandbox policy is unavailable")
	}
	replacement := mcp.NewManager()
	replacement.SetSandbox(a.services.sandbox.Snapshot())
	replacement.RegisterTools(a.services.registry)
	a.services.mcp = replacement
	for name := range a.discovery.deferTools {
		if len(name) >= 5 && name[:5] == "mcp__" {
			delete(a.discovery.deferTools, name)
			delete(a.discovery.discovered, name)
		}
	}
	if clearGrants {
		clear(a.capabilities.capabilityGrants)
		a.capabilities.capabilityRevision++
		a.capabilities.revocationRevision++
	}
	a.mu.Unlock()
	return a.resumeSandboxAdmission()
}

// applySettingsSnapshot mutates live runtime state to match a target snapshot. It
// is the single application point for every settings command, so a setting
// behaves identically however it was admitted. Side effects that must run outside
// a.mu (client routing, permission manager, hook context, events) follow the
// critical section.
func (a *Agent) applySettingsSnapshot(target session.Settings, binding *modelBinding, revision uint64) {
	a.mu.Lock()
	modelChanged := binding != nil
	permModeChanged := target.PermissionPolicy != a.services.cfg.PermissionMode
	allowChanged := !equalStringSlice(target.AlwaysAllow, a.services.cfg.AlwaysAllow)
	denyChanged := !equalStringSlice(target.AlwaysDeny, a.services.cfg.AlwaysDeny)
	sandboxChanged := target.NetworkAccess != a.services.cfg.NetworkAccess || target.LocalDevelopmentNetwork != a.services.cfg.LocalDevelopmentNetwork ||
		!equalStringSlice(target.AdditionalDirectories, a.services.cfg.AdditionalDirectories) ||
		!equalStringSlice(target.AdditionalReadOnlyDirectories, a.services.cfg.AdditionalReadOnlyDirectories) ||
		!equalStringSlice(target.DisallowedDirectories, a.services.cfg.DisallowedDirectories)
	permChanged := permModeChanged || allowChanged || denyChanged

	mode, modeErr := permissions.ParseMode(target.PermissionPolicy)
	if modeErr != nil {
		// Invalid persisted policies cannot grant automatic execution.
		mode = permissions.ModeManual
	}

	// generation
	a.services.cfg.EnableProjectMemory, a.services.baseCfg.EnableProjectMemory = target.ProjectMemoryEnabled, target.ProjectMemoryEnabled
	a.services.cfg.MemoryReadOnly, a.services.baseCfg.MemoryReadOnly = target.MemoryReadOnly, target.MemoryReadOnly
	a.services.cfg.MemoryHistory, a.services.baseCfg.MemoryHistory = target.MemoryHistory, target.MemoryHistory
	a.cancelDisabledMemoryHistoryLocked()
	a.services.cfg.ReasoningEffort, a.services.baseCfg.ReasoningEffort = target.ReasoningEffort, target.ReasoningEffort
	a.services.cfg.Verbosity, a.services.baseCfg.Verbosity = target.Verbosity, target.Verbosity

	// permission
	a.services.cfg.PermissionMode = string(mode)
	a.services.baseCfg.PermissionMode = string(mode)
	a.services.cfg.AlwaysAllow = cloneStringSlice(target.AlwaysAllow)
	a.services.cfg.AlwaysDeny = cloneStringSlice(target.AlwaysDeny)
	a.services.baseCfg.AlwaysAllow = cloneStringSlice(target.AlwaysAllow)
	a.services.baseCfg.AlwaysDeny = cloneStringSlice(target.AlwaysDeny)

	// sandbox
	a.services.cfg.NetworkAccess = target.NetworkAccess
	a.services.cfg.LocalDevelopmentNetwork = target.LocalDevelopmentNetwork
	a.services.cfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.services.cfg.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
	a.services.cfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	a.services.baseCfg.NetworkAccess = target.NetworkAccess
	a.services.baseCfg.LocalDevelopmentNetwork = target.LocalDevelopmentNetwork
	a.services.baseCfg.AdditionalDirectories = cloneStringSlice(target.AdditionalDirectories)
	a.services.baseCfg.AdditionalReadOnlyDirectories = cloneStringSlice(target.AdditionalReadOnlyDirectories)
	a.services.baseCfg.DisallowedDirectories = cloneStringSlice(target.DisallowedDirectories)
	if sandboxChanged {
		cfgCopy := cloneConfig(a.services.cfg)
		newSandbox := rebuildSandbox(&cfgCopy, a.services.cfg.Workspace, a.services.sandbox)
		a.services.sandbox = newSandbox
		a.services.sandbox.Revision = revision
		// This is the monotonic sandbox-approval epoch. Do not rely only on the
		// backend snapshot Revision, which may start above settingsRev and later
		// be replaced by it.
		a.capabilities.capabilityRevision++
		a.capabilities.revocationRevision++
	}

	// model
	if binding != nil {
		a.applyBindingLocked(*binding)
	}

	a.sequences.settingsRev = revision
	routeCfg := cloneConfig(a.services.cfg)
	a.mu.Unlock()

	if binding != nil {
		a.registerBindingRoutes(*binding, routeCfg)
	}
	var policy *permissions.Policy
	if permChanged {
		policy = &permissions.Policy{
			AlwaysAllow: cloneStringSlice(target.AlwaysAllow),
			AlwaysDeny:  cloneStringSlice(target.AlwaysDeny),
		}
	}
	if permChanged || sandboxChanged {
		a.syncSettingsPolicy(mode, policy)
	}
	if modelChanged {
		a.emit(protocol.EventView{Kind: protocol.EventStateChanged, Text: "model:" + binding.model})
		a.emitStatus("model switched to %s", binding.model)
	}
	if permChanged {
		a.emit(protocol.EventView{Kind: protocol.EventStateChanged, PermissionMode: string(mode)})
		a.emitStatus("permission policy updated")
	}
	if sandboxChanged {
		a.emit(protocol.EventView{Kind: protocol.EventStateChanged})
		a.emitStatus("sandbox policy updated (network: %v, read/write roots: %d, read-only roots: %d)",
			target.NetworkAccess, len(target.AdditionalDirectories), len(target.AdditionalReadOnlyDirectories))
	}
	a.publishState()
}

// syncSettingsPolicy publishes authorization state after the settings mutation.
// Callers hold settingsCommitMu, but not a.mu. A nil policy preserves rules
// when only the execution mode or sandbox changed.
func (a *Agent) syncSettingsPolicy(mode permissions.Mode, policy *permissions.Policy) {
	a.services.perms.SetMode(mode)
	if policy != nil {
		a.services.perms.SetPolicy(*policy)
	}
	a.syncHookContext()
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
