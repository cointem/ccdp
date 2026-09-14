package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

// History returns a copy of the conversation history.
func (a *Agent) History() []messages.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]messages.Message, len(a.history))
	copy(out, a.history)
	return out
}

// ReloadSettings re-reads the per-project settings (.ccdp/settings.json +
// settings.local.json) and applies the workspace-scoped keys at runtime
// (Claude Code's settings hot-reload): permission policy, hooks, sandbox mode.
func (a *Agent) ReloadSettings() error {
	workspace, err := a.reserveSettingsMutation()
	if err != nil {
		return err
	}
	defer a.releaseSettingsMutation()
	if err := a.reloadSettingsContext(a.rootCtx, workspace); err != nil {
		return err
	}
	a.emitStatus("settings reloaded from .ccdp/settings.json")
	return nil
}

type settingsCandidate struct {
	cfg config.Config
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
	base := a.baseCfg.Clone()
	store := a.trustStore
	active := a.activeBinding
	models := a.models
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
	if _, err := sandbox.ParseMode(effective.SandboxMode); err != nil {
		return nil, err
	}
	if _, err := filepath.Abs(workspace); err != nil {
		return nil, err
	}
	sb := buildSandbox(&effective, workspace)
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
		hooks: cloneHookConfig(effective.Hooks), webTools: effective.WebToolsEnabled(),
		tools: cloneToolSpecs(effective.Tools), customPerms: customPerms,
		mcpServers: cloneMCPConfig(effective.MCPServers)}, nil
}

func (a *Agent) reloadSettingsContext(ctx context.Context, workspace string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	candidate, err := a.prepareSettingsCandidate(workspace)
	if err != nil {
		return err
	}
	if err := a.commitSettingsCandidateContext(ctx, candidate); err != nil {
		return err
	}
	a.emit(Event{Type: EventModeChanged, Mode: candidate.mode})
	a.emit(Event{Type: EventSandboxChanged})
	return nil
}

// TrustProject records an explicit user authorization for the current
// workspace's executable project settings in the external trust store. The
// operation does not run hooks or start MCP; callers should reload after a
// successful authorization to prepare and publish the candidate.
func (a *Agent) TrustProject() error {
	a.mu.Lock()
	workspace := a.cfg.Workspace
	store := a.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	project, err := config.LoadProjectSettings(workspace)
	if err != nil {
		return err
	}
	if _, err := store.AuthorizeProject(workspace, project); err != nil {
		return err
	}
	return nil
}

// RevokeProjectTrust removes the explicit external authorization. Current
// bindings remain until an idle ReloadSettings publishes the now-safe
// candidate; this avoids mutating a live step in place.
func (a *Agent) RevokeProjectTrust() error {
	a.mu.Lock()
	workspace := a.cfg.Workspace
	store := a.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	return store.RevokeProject(workspace)
}

// trustProjectContext applies or revokes project trust and publishes the
// corresponding settings candidate as one idle operation. If candidate
// preparation fails, restore the previous trust record so an authorization
// command cannot leave an unexplained half-applied state.
func (a *Agent) trustProjectContext(ctx context.Context, revoke bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	a.mu.Lock()
	workspace := a.cfg.Workspace
	store := a.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	project, err := config.LoadProjectSettings(workspace)
	if err != nil {
		return err
	}
	trusted, _, err := store.Check(workspace, project)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if revoke {
		if err := store.RevokeProject(workspace); err != nil {
			return err
		}
		if err := a.reloadSettingsContext(ctx, workspace); err != nil {
			if trusted {
				_, _ = store.AuthorizeProject(workspace, project)
			}
			return err
		}
		return nil
	}
	if _, err := store.AuthorizeProject(workspace, project); err != nil {
		return err
	}
	if err := a.reloadSettingsContext(ctx, workspace); err != nil {
		if trusted {
			_, _ = store.AuthorizeProject(workspace, project)
		} else {
			_ = store.RevokeProject(workspace)
		}
		return err
	}
	return nil
}

func (a *Agent) commitSettingsCandidate(candidate *settingsCandidate) error {
	return a.commitSettingsCandidateContext(a.rootCtx, candidate)
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
	oldCfg := a.cfg.Clone()
	oldMCP := cloneMCPConfig(oldCfg.MCPServers)
	binding := candidate.binding
	if binding.client == nil {
		// Keep compatibility with hand-built Agents/tests that construct a
		// candidate directly. Even on that path, adapt the frozen provider to
		// the candidate's effective operator limits before the next step.
		binding = a.activeBinding
		binding.client = providerWithOperatorCaps(candidate.cfg, binding.client)
	}
	if binding.model == "" {
		binding.model = a.activeBinding.model
	}
	planMode := a.planMode || candidate.mode == permissions.ModePlan
	permissionMode := candidate.mode
	if planMode {
		permissionMode = a.planBaseMode
		if permissionMode == "" || permissionMode == permissions.ModePlan {
			permissionMode = permissions.ModeDefault
		}
	}
	candidate.binding = binding
	settings := a.settingsForCandidateLocked(candidate, permissionMode, planMode)
	workflow := session.WorkflowState{Phase: string(a.workflow)}
	if planMode {
		if workflow.Phase == "" || workflow.Phase == string(protocol.WorkflowOff) {
			workflow.Phase = string(protocol.WorkflowDrafting)
		}
	} else {
		workflow = session.WorkflowState{Phase: string(protocol.WorkflowOff)}
	}
	nextRevision := a.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	a.mu.Unlock()

	// Start and handshake candidate MCP resources without publishing them. The
	// durable settings fact below is the admission point; Abort closes every
	// started candidate if that write fails and leaves the old generation live.
	var mcpCandidate *mcp.RefreshCandidate
	if !reflect.DeepEqual(oldMCP, candidate.mcpServers) {
		var err error
		mcpCandidate, err = a.mcp.PrepareRefresh(ctx, candidate.mcpServers, candidate.sandbox, true)
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

	// SettingsChanged/WorkflowChanged are committed before any executable
	// extension is made visible. A journal failure therefore cannot leave a
	// live candidate whose durable state says the old settings were active.
	if err := a.persistSettingsWorkflowFact(settings, nextRevision, &workflow); err != nil {
		return err
	}
	if mcpCandidate != nil {
		if err := mcpCandidate.Commit(); err != nil {
			// refreshMu makes this impossible while the Agent owns the candidate,
			// but surface it as a hard failure rather than claiming a publication.
			return fmt.Errorf("commit MCP candidate: %w", err)
		}
		abortCandidate = false
	}
	a.mcp.SetSandbox(candidate.sandbox)
	// A generated HTTP binding may have a new endpoint/key or a newly wrapped
	// capability ceiling. Publish it into the mutable registry only after the
	// durable settings admission; explicit plugin routes remain owned by their
	// plugin and are represented by the binding itself.
	if binding.client != nil && binding.routeKind == "http" && a.models != nil {
		a.models.Register(binding.client)
		endpoint, apiKey := candidate.cfg.EndpointFor(binding.model)
		a.models.RouteDefault(binding.model, binding.client.Name(), endpoint, apiKey)
	}

	// Registry registration is infallible after validation. Publish each
	// project-owned scope as one complete batch; built-ins/plugin scopes and any
	// frozen step leases remain untouched until their turn ends.
	customTools := make([]tools.Tool, 0, len(candidate.tools))
	customPerms := make(map[string]string, len(candidate.customPerms))
	for _, spec := range candidate.tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		customTools = append(customTools, tools.NewCommandTool(name, spec.Description, spec.Command, spec.InputSchema))
		customPerms[name] = candidate.customPerms[name]
	}
	webTools := make([]tools.Tool, 0, 2)
	if candidate.webTools {
		webTools = append(webTools, tools.NewWebFetchTool(), tools.NewWebSearchTool())
	}
	a.registry.ReplaceScopes(map[string][]tools.Tool{"custom": customTools, "web": webTools})
	a.mu.Lock()
	a.customPerms = customPerms
	*a.cfg = candidate.cfg
	if binding.client != nil {
		a.activeBinding = binding
		a.client = binding.client
		a.primaryClient = binding.client
		a.primaryEndpoint = binding.endpoint
		a.primaryRouteKind = binding.routeKind
		a.activeModel = binding.model
	}
	a.sandbox = candidate.sandbox
	a.planMode = planMode
	a.planBaseMode = permissionMode
	a.workflow = protocol.WorkflowState(workflow.Phase)
	a.cfg.PermissionMode = string(permissionMode)
	a.settingsRev = nextRevision
	a.mu.Unlock()
	a.perms.SetMode(permissionMode)
	a.perms.SetPolicy(candidate.cfg.PermissionPolicy())
	a.hooks.Update(cloneHookConfig(candidate.hooks))
	a.hooks.SetSandbox(candidate.sandbox)
	// Keep checkpoint git operations on the newly published boundary after a
	// reload or /cd. Typed operations still pass their own cancellable context
	// through CreateContext/RestoreContext below.
	a.checkpoints.SetExecutionBoundary(a.rootCtx, candidate.sandbox)
	a.syncHookContext()
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
		binding = a.activeBinding
	}
	if binding.model != "" {
		settings.Model = binding.model
		settings.Provider = binding.provider
		settings.Endpoint, _ = sanitizeRequestEndpoint(binding.endpoint)
	}
	settings.PermissionPolicy = string(permissionMode)
	settings.AlwaysAllow = append([]string(nil), candidate.cfg.AlwaysAllow...)
	settings.AlwaysDeny = append([]string(nil), candidate.cfg.AlwaysDeny...)
	settings.SandboxPolicy = candidate.cfg.SandboxMode
	settings.AllowNetwork = candidate.cfg.SandboxAllowNetwork
	settings.AllowNetworkSet = true
	settings.AdditionalDirectories = append([]string(nil), candidate.cfg.AdditionalDirectories...)
	settings.DisallowedDirectories = append([]string(nil), candidate.cfg.DisallowedDirectories...)
	settings.ContextWindow = candidate.cfg.ContextWindow
	settings.CompactThreshold = candidate.cfg.CompactThreshold
	settings.MaxResultSizeChars = candidate.cfg.MaxResultSizeChars
	settings.MaxTurns = candidate.cfg.MaxTurns
	settings.MaxBudgetUSD = candidate.cfg.MaxBudgetUSD
	settings.MaxReplyTokens = candidate.cfg.MaxReplyTokens
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

func (a *Agent) setWebTools(enabled bool) {
	var webTools []tools.Tool
	if enabled {
		webTools = []tools.Tool{tools.NewWebFetchTool(), tools.NewWebSearchTool()}
	}
	a.registry.ReplaceScope("web", webTools)
}

func cloneHookConfig(src hooks.Config) hooks.Config {
	dst := hooks.Config{}
	for event, specs := range src {
		dst[event] = append([]hooks.HookSpec(nil), specs...)
	}
	return dst
}

// SetWorkspace changes the agent's working directory at runtime (Codex /cd),
// rebuilding the sandbox and invalidating the workspace model. The sandbox
// pointer and workspace are swapped atomically under a.mu; tool goroutines
// read them via currentSandbox(), so a /cd during a running turn never gives
// half of one batch the old directory and half the new one.
func (a *Agent) SetWorkspace(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	if _, err := a.reserveSettingsMutation(); err != nil {
		return err
	}
	defer a.releaseSettingsMutation()
	if err := a.setWorkspaceContext(a.rootCtx, abs); err != nil {
		return err
	}
	a.emitStatus("workspace → %s", abs)
	return nil
}

func (a *Agent) setWorkspaceContext(ctx context.Context, abs string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory: %s", abs)
	}
	candidate, err := a.prepareSettingsCandidate(abs)
	if err != nil {
		return err
	}
	if err := a.commitSettingsCandidateContext(ctx, candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.baseCfg.Workspace = abs
	a.cfg.Workspace = abs
	a.mu.Unlock()
	a.wsInfo = nil
	return nil
}

// buildSandbox assembles the runtime sandbox for dir from config (mirrors
// config.Config.Sandbox but for an arbitrary workspace, without touching the
// config package).
func buildSandbox(cfg *config.Config, dir string) *sandbox.Sandbox {
	mode, err := sandbox.ParseMode(cfg.SandboxMode)
	if err != nil {
		mode = sandbox.ModeConfine
	}
	s := sandbox.New(dir, mode)
	if cfg.SandboxLimits != nil {
		lim := *cfg.SandboxLimits
		s.Limits = &lim
	}
	s.AllowNetwork = cfg.SandboxAllowNetwork
	for _, d := range cfg.AdditionalDirectories {
		s.AddDir(d)
	}
	for _, d := range cfg.DisallowedDirectories {
		s.AddDisallowedDir(d)
	}
	if cfg.SessionDir != "" {
		s.AddDisallowedDir(cfg.SessionDir)
		s.AddProtectedDir(cfg.SessionDir)
	}
	s.AddProtectedDir(config.ProjectSettingsDir(dir))
	if source := cfg.SourcePath(); source != "" {
		s.AddProtectedDir(source)
	}
	if cfg.TrustStore != nil && cfg.TrustStore.Path != "" {
		s.AddProtectedDir(cfg.TrustStore.Path)
	}
	return s
}

// reserveSettingsMutation closes the check-to-publish race for /reload and
// /cd. The reservation is represented by the existing busy owner state, so
// input admission queues rather than starting a turn and model/policy
// commands cannot observe a half-published candidate.
func (a *Agent) reserveSettingsMutation() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy || a.settling || a.closing || a.closed {
		return "", fmt.Errorf("settings mutation requires an idle session")
	}
	workspace := a.cfg.Workspace
	a.busy = true
	a.phase = protocol.PhasePreparing
	return workspace, nil
}

func (a *Agent) releaseSettingsMutation() {
	a.mu.Lock()
	if a.busy && a.turnCancel == nil && !a.closing && !a.closed {
		a.busy = false
		a.phase = protocol.PhaseIdle
	}
	a.mu.Unlock()
	a.publishState()
}

// ExportMarkdown renders the session as a markdown transcript (Codex /export).
func (a *Agent) ExportMarkdown() string {
	a.mu.Lock()
	hist := make([]messages.Message, len(a.history))
	copy(hist, a.history)
	model, ws := a.cfg.Model, a.cfg.Workspace
	a.mu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "# ccdp session %s\n\n", a.sessionID)
	fmt.Fprintf(&sb, "- Model: %s\n- Workspace: `%s`\n- Exported: %s\n\n---\n\n",
		model, ws, time.Now().Format("2006-01-02 15:04:05"))

	for _, m := range hist {
		switch m.Role {
		case messages.RoleUser:
			fmt.Fprintf(&sb, "## User\n\n%s\n\n", m.Content)
		case messages.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n", m.Content)
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&sb, "\n_→ %s(%s)_\n", tc.Name, strings.TrimSpace(messages.MarshalArguments(tc.Arguments)))
				}
				sb.WriteString("\n")
			} else {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n\n", m.Content)
			}
		case messages.RoleTool:
			fmt.Fprintf(&sb, "### Tool result\n\n```text\n%s\n```\n\n", m.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}
