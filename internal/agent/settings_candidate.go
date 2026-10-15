package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

type settingsCandidate struct {
	workspaceChange bool
	cfg             config.Config
	// sourceSandboxRevision binds preparation to the live policy generation it
	// observed. A candidate must not overwrite a policy published while its
	// project/provider inputs were being resolved outside settingsCommitMu.
	sourceSandboxRevision uint64
	// sourceSettingsRevision is the monotonic publication epoch. Sandbox.Revision
	// is also used by standalone snapshots and can legitimately be reset to a
	// lower settings revision after initialization, so it cannot prevent ABA on
	// its own.
	sourceSettingsRevision uint64
	// binding is resolved against the candidate config and a cloned provider
	// registry. Keeping it with cfg prevents a reload that changes context or
	// output limits from leaving the active binding's capability wrapper on the
	// previous generation.
	binding     modelBinding
	mode        permissions.Mode
	sandbox     *sandbox.Sandbox
	hooks       hooks.Config
	webTools    bool
	tools       []config.ToolSpec
	customPerms map[string]string
	mcpServers  map[string]mcp.ServerConfig
}

func (a *Agent) prepareSettingsCandidate(workspace string) (*settingsCandidate, error) {
	a.mu.Lock()
	base := a.services.baseCfg.Clone()
	store := a.services.trustStore
	active := a.modelsState.activeBinding
	var models *plugin.ModelRegistry
	if a.services.models != nil {
		models = a.services.models.Freeze()
	}
	oldSandbox := a.services.sandbox
	sourceSettingsRevision := a.sequences.settingsRev
	sourceSandboxRevision := uint64(0)
	if oldSandbox != nil {
		sourceSandboxRevision = oldSandbox.Snapshot().Revision
	}
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	base.Workspace = workspace
	project, err := config.LoadProjectSettingsTrusted(workspace, store)
	if err != nil {
		return nil, err
	}
	effective := base
	config.ApplyProjectSettings(&effective, &project)
	effective.Workspace = workspace
	// Reload/cd are workspace/config mutations, not model-selection commands.
	// Keep the active model identity, but resolve its provider again against a
	// cloned catalog so an endpoint/key change cannot reuse a stale generated
	// HTTP adapter. This also rebuilds the operator capability wrapper when the
	// effective context/output limits change.
	if active.model != "" {
		effective.Model = active.model
	}
	if err := effective.Validate(); err != nil {
		return nil, err
	}
	mode, err := permissions.ParseMode(effective.PermissionMode)
	if err != nil {
		return nil, err
	}
	if _, err := filepath.Abs(workspace); err != nil {
		return nil, err
	}
	sb := buildSandbox(&effective, workspace)
	sb.ShareExecutionGuard(oldSandbox)
	customPerms := make(map[string]string)
	for _, spec := range effective.Tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		customPerms[name] = strings.ToLower(spec.Permission)
	}
	var binding modelBinding
	if active.model != "" {
		var modelRegistry *plugin.ModelRegistry
		if models != nil {
			modelRegistry = models.Clone()
		}
		binding, err = providerBinding(effective, modelRegistry, active.model, active.version+1)
		if err != nil {
			return nil, err
		}
	} else {
		// Package tests and embedders may construct a minimal Agent without an
		// active binding. The commit path retains that compatibility and will
		// leave its current provider untouched.
		binding = active
	}
	return &settingsCandidate{cfg: effective, binding: binding, mode: mode, sandbox: sb,
		sourceSandboxRevision: sourceSandboxRevision, sourceSettingsRevision: sourceSettingsRevision,
		hooks: cloneHookConfig(effective.Hooks), webTools: effective.WebToolsEnabled(),
		tools: cloneToolSpecs(effective.Tools), customPerms: customPerms,
		mcpServers: cloneMCPConfig(effective.MCPServers)}, nil
}

func (a *Agent) commitSettingsCandidate(candidate *settingsCandidate) error {
	return a.commitSettingsCandidateContext(a.lifecycle.rootCtx, candidate)
}

func (a *Agent) commitSettingsCandidateContext(ctx context.Context, candidate *settingsCandidate) error {
	if candidate == nil {
		return fmt.Errorf("nil settings candidate")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// Hold the publication lock for the whole candidate transaction. Queries
	// still run while an operation is busy, but they must see either the old
	// config/catalog or the new one, never the interval between those swaps.
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	a.mu.Lock()
	if a.capabilities.capabilityRevoking {
		a.mu.Unlock()
		return fmt.Errorf("sandbox capability revocation is in progress")
	}
	oldCfg := a.services.cfg.Clone()
	oldSandbox := a.services.sandbox
	sandboxPolicyChanged := oldCfg.NetworkProxy != candidate.cfg.NetworkProxy || oldCfg.NetworkAccess != candidate.cfg.NetworkAccess ||
		!equalStringSlice(oldCfg.AdditionalDirectories, candidate.cfg.AdditionalDirectories) ||
		!equalStringSlice(oldCfg.AdditionalReadOnlyDirectories, candidate.cfg.AdditionalReadOnlyDirectories) ||
		!equalStringSlice(oldCfg.DisallowedDirectories, candidate.cfg.DisallowedDirectories)
	currentSandboxRevision := uint64(0)
	if oldSandbox != nil {
		currentSandboxRevision = oldSandbox.Snapshot().Revision
	}
	if currentSandboxRevision != candidate.sourceSandboxRevision || a.sequences.settingsRev != candidate.sourceSettingsRevision {
		a.mu.Unlock()
		return fmt.Errorf("settings changed while candidate was being prepared")
	}
	oldMCP := cloneMCPConfig(oldCfg.MCPServers)
	binding := candidate.binding
	if binding.client == nil {
		// Keep compatibility with hand-built Agents/tests that construct a
		// candidate directly. Even on that path, adapt the frozen provider to
		// the candidate's effective operator limits before the next step.
		binding = a.modelsState.activeBinding
		binding.client = providerWithOperatorCaps(candidate.cfg, candidate.cfg.Model, binding.client)
	}
	if binding.model == "" {
		binding.model = a.modelsState.activeBinding.model
	}
	planMode := a.plan.planMode || candidate.mode == permissions.ModePlan
	permissionMode := candidate.mode
	if planMode {
		permissionMode = a.plan.planBaseMode
		if permissionMode == "" || permissionMode == permissions.ModePlan {
			permissionMode = permissions.ModeDefault
		}
	}
	candidate.binding = binding
	settings := a.settingsForCandidateLocked(candidate, permissionMode, planMode)
	workflow := session.WorkflowState{Phase: string(a.lifecycle.workflow)}
	if planMode {
		if workflow.Phase == "" || workflow.Phase == string(protocol.WorkflowOff) {
			workflow.Phase = string(protocol.WorkflowDrafting)
		}
	} else {
		workflow = session.WorkflowState{Phase: string(protocol.WorkflowOff)}
	}
	nextRevision := a.sequences.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	a.mu.Unlock()

	policyTightened := oldSandbox != nil && sandbox.PolicyTightened(oldSandbox, candidate.sandbox)
	quiesced := false
	var quiescenceRevision uint64
	durable := false
	policyPublished := false
	if policyTightened {
		// Tightening always invalidates and joins old work. The sticky witness
		// controls whether success can be confirmed, not whether this protocol
		// runs at all.
		revision, err := a.quiesceForSandboxSettingsChange(ctx, false)
		if err != nil {
			return fmt.Errorf("sandbox settings change cannot be confirmed because old-policy descendants may remain: %w", err)
		}
		quiesced = true
		quiescenceRevision = revision
	}
	defer func() {
		if !quiesced || durable || policyPublished {
			return
		}
		a.settingsCommitMu.Unlock()
		a.abortQuiescence(quiescenceRevision)
		a.settingsCommitMu.Lock()
	}()

	// Start and handshake candidate MCP resources without publishing them. The
	// durable settings fact below is the admission point; Abort closes every
	// started candidate if that write fails and leaves the old generation live.
	a.mu.Lock()
	capabilityRevision, manager := a.capabilities.capabilityRevision, a.services.mcp
	a.mu.Unlock()
	var mcpCandidate *mcp.RefreshCandidate
	if !reflect.DeepEqual(oldMCP, candidate.mcpServers) || sandboxPolicyChanged || !a.services.mcp.Matches(candidate.mcpServers) {
		var err error
		a.settingsCommitMu.Unlock()
		mcpCandidate, err = manager.PrepareRefresh(ctx, candidate.mcpServers, candidate.sandbox, true)
		a.settingsCommitMu.Lock()
		if err != nil {
			return err
		}
	}
	abortCandidate := true
	defer func() {
		if abortCandidate && mcpCandidate != nil {
			_ = mcpCandidate.Abort()
		}
	}()

	// Hand the complete candidate to the owner after preparation. The worker
	// owns Abort until the owner acknowledges durable installation.
	install := settingsCandidateInstall{ctx: ctx, candidate: candidate, binding: binding,
		settings: settings, workflow: workflow, nextRevision: nextRevision,
		planMode: planMode, permissionMode: permissionMode,
		policyTightened: policyTightened, sandboxPolicyChanged: sandboxPolicyChanged,
		mcpCandidate: mcpCandidate, manager: manager, capabilityRevision: capabilityRevision,
		result: make(chan error, 1)}
	a.settingsCommitMu.Unlock()
	a.ensureRun()
	select {
	case a.owner.control <- install:
	case <-ctx.Done():
		a.settingsCommitMu.Lock()
		return ctx.Err()
	case <-a.owner.done:
		a.settingsCommitMu.Lock()
		return fmt.Errorf("session owner is closed")
	}
	var err error
	select {
	case err = <-install.result:
	case <-a.owner.done:
		err = fmt.Errorf("session closed before candidate installation settled")
	}
	if mcpCandidate != nil {
		mcpCandidate.FinishRetirement()
	}
	a.settingsCommitMu.Lock()
	if err != nil {
		return err
	}
	durable, policyPublished, abortCandidate = true, true, false
	return nil
}

// settingsForCandidateLocked converts a resolved candidate into the
// credential-free session settings projection. It intentionally uses the
// currently active provider binding for model identity: reload/cd do not
// silently switch a live model just because a project file changed.
func (a *Agent) settingsForCandidateLocked(candidate *settingsCandidate, permissionMode permissions.Mode, planMode bool) session.Settings {
	settings := a.sessionSettingsLocked()
	if candidate == nil {
		return settings
	}
	settings.Workspace = candidate.cfg.Workspace
	binding := candidate.binding
	if binding.model == "" {
		binding = a.modelsState.activeBinding
	}
	if binding.model != "" {
		settings.Model = binding.model
		settings.Provider = binding.provider
		settings.Endpoint, _ = sanitizeRequestEndpoint(binding.endpoint)
	}
	settings.PermissionPolicy = string(permissionMode)
	settings.AlwaysAllow = append([]string(nil), candidate.cfg.AlwaysAllow...)
	settings.AlwaysDeny = append([]string(nil), candidate.cfg.AlwaysDeny...)
	settings.NetworkAccess = candidate.cfg.NetworkAccess
	settings.AdditionalDirectories = append([]string(nil), candidate.cfg.AdditionalDirectories...)
	settings.AdditionalReadOnlyDirectories = append([]string(nil), candidate.cfg.AdditionalReadOnlyDirectories...)
	settings.DisallowedDirectories = append([]string(nil), candidate.cfg.DisallowedDirectories...)
	settings.ContextWindow = candidate.cfg.ContextWindow
	settings.EffectiveContextWindow = candidate.cfg.ContextWindowFor(binding.model)
	settings.CompactThreshold = candidate.cfg.CompactThreshold
	settings.MaxResultSizeChars = candidate.cfg.MaxResultSizeChars
	settings.MaxTurns = candidate.cfg.MaxTurns

	maxOutputTokens := candidate.cfg.MaxOutputTokensFor(binding.model)
	settings.MaxOutputTokens = &maxOutputTokens
	settings.ReasoningEffort = candidate.cfg.ReasoningEffort
	settings.Verbosity = candidate.cfg.Verbosity
	settings.GenerationOptionsSet = true
	settings.MaxToolOutputCharsPerTurn = candidate.cfg.MaxToolOutputCharsPerTurn
	settings.ExecutionMode = "execute"
	if planMode {
		settings.ExecutionMode = "plan"
	}
	return settings
}

func cloneToolSpecs(src []config.ToolSpec) []config.ToolSpec {
	out := append([]config.ToolSpec(nil), src...)
	for i := range out {
		if out[i].InputSchema != nil {
			out[i].InputSchema = cloneMap(out[i].InputSchema)
		}
	}
	return out
}

func cloneMCPConfig(src map[string]mcp.ServerConfig) map[string]mcp.ServerConfig {
	out := make(map[string]mcp.ServerConfig, len(src))
	for name, cfg := range src {
		cfg.Args = append([]string(nil), cfg.Args...)
		if cfg.Env != nil {
			env := make(map[string]string, len(cfg.Env))
			for k, v := range cfg.Env {
				env[k] = v
			}
			cfg.Env = env
		}
		out[name] = cfg
	}
	return out
}

func cloneHookConfig(src hooks.Config) hooks.Config {
	dst := hooks.Config{}
	for event, specs := range src {
		dst[event] = append([]hooks.HookSpec(nil), specs...)
	}
	return dst
}
