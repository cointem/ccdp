package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccdp/internal/checkpoint"
	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// Agent is the interactive coding agent. It owns the conversation history, the
// tool registry, the permission gate and the event/control channels that
// connect it to the TUI. It also hosts the plugin system: the built-in toolset
// and default LLM provider are loaded as plugins, and external plugins can
// register tools, providers, Go hooks and session callbacks at runtime.
//
// Concurrency model:
//   - Run() is the sole consumer of the control channel and dispatches work.
//   - Each turn runs in its own goroutine; at most one turn is active.
//   - Approval answers travel on approvalResp; interrupts are delivered via
//     turnCancel + an interrupt flag.
type Agent struct {
	cfg      *config.Config
	client   *llm.Client
	registry *tools.Registry
	perms    *permissions.Manager
	hooks    *hooks.Manager
	sandbox  *sandbox.Sandbox
	wsInfo   *workspace.Info // cached workspace model

	// Plugin host and domain registries.
	host        *plugin.Host
	evbus       *events.Bus
	models      *plugin.ModelRegistry
	gohooks     *plugin.GoHooks
	sessionReg  *plugin.SessionRegistry
	mcp         *mcp.Manager
	skills      *skills.Store
	checkpoints *checkpoint.Store

	sessionID string
	createdAt time.Time
	history   []messages.Message

	events chan Event   // agent → UI
	ctrl   chan Control // UI → agent

	// Turn lifecycle (guarded by mu where cross-goroutine).
	mu            sync.Mutex
	busy          bool
	interruptFlag bool
	stop          bool
	turnCancel    context.CancelFunc
	turnCtx       context.Context
	pendingMsgs   []string

	// Pending approval (guarded by mu).
	pendingApproval *ApprovalRequest
	approvalResp    chan approvalAnswer

	// Plan mode (Claude Code's /plan): while on, the model only proposes a plan;
	// mutating tools are blocked until the user approves it.
	planMode bool
	planResp chan bool

	// customPerms holds per-custom-tool permission overrides from config
	// ("allow" / "deny" / "ask").
	customPerms map[string]string

	// deferTools are tools not injected inline (MCP + config custom tools);
	// the model discovers them via ToolSearch. discovered tracks which of
	// them have been surfaced this session.
	deferTools map[string]bool
	discovered map[string]bool

	// sessionStartAt fixes the environment "Date" once per session so the
	// system prompt prefix stays byte-identical across turns (prompt cache).
	sessionStartAt time.Time

	// modelSwitchMsg is injected into the next request's system prompt after a
	// model switch (Codex's ModelSwitchInstructions), then cleared.
	modelSwitchMsg string

	// fallbackClient is used when the primary model fails (Claude's
	// --fallback-model). usedFallback latches per turn so each turn gets one
	// fallback attempt. activeModel tracks the model actually in use so usage
	// is priced with the right rate card after a fallback switch.
	fallbackClient *llm.Client
	usedFallback   bool
	activeModel    string

	// outputBudget is the running count of tool-result characters delivered to
	// the model this turn (per-turn aggregate limit).
	outputBudget int

	// tokenBaseline anchors compaction estimates on the provider's real prompt
	// token count (Codex's BodyAfterPrefix idea): lastPromptTokens was reported
	// for a request built from baselineLen history messages; tokens for history
	// grown since then are estimated locally. Reset when history is rewritten.
	tokenBaseline struct {
		promptTokens int
		historyLen   int
	}

	// guardianUses counts guardian reviews this turn (circuit breaker ≤ 3).
	guardianUses int

	// AutoMem per-turn tracking (guarded by mu): the current user request, the
	// files touched, and the assistant's final summary. recordMemory (in
	// turnFinished) folds these into the session memory log.
	turnUserMsg string
	turnTouched []string
	turnSummary string

	// interruptNote is injected into the next request's system prompt once
	// (Codex's INTERRUPTED_GUIDANCE): after a user interrupt the model must
	// know that tools may have partially executed and verify state.
	interruptNote bool

	// trace is the JSONL session trace file (nil when tracing is off).
	trace   *os.File
	traceMu sync.Mutex

	// approvalMu serializes approval requests so parallel tool workers never
	// clobber the single pending-approval slot.
	approvalMu sync.Mutex

	// usage accumulates token/cost accounting (guarded by mu).
	usage Usage
}

type approvalAnswer struct {
	approve  bool
	remember bool
}

// New creates an agent with the given config and channels. The extension host
// is set up here: a domain event bus, a scoped tool registry, and the built-in
// plugins (toolset + default provider) are loaded through the same path an
// external plugin would use.
func New(cfg *config.Config, evCh chan Event, ctrl chan Control) (*Agent, error) {
	baseURL, apiKey := cfg.EndpointFor(cfg.Model)
	client, err := llm.NewClient(llm.Config{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   cfg.Model,
		Timeout: 10 * time.Minute,
		Debug:   cfg.Debug,
	})
	if err != nil {
		return nil, err
	}

	// Extension host: domain registries + plugins.
	bus := events.NewBus()
	registry := tools.NewRegistry()
	ectx := &plugin.Context{
		Events:  bus,
		Tools:   registry,
		Models:  plugin.NewModelRegistry(),
		Hooks:   plugin.NewGoHooks(),
		Session: plugin.NewSessionRegistry(),
	}
	host := plugin.NewHost(ectx)
	registry.SetChangeListener(func() {
		bus.Emit(events.TopicToolChange, events.ToolEvent{ToolName: ""})
	})
	if err := host.Load(plugin.NewToolsPlugin(cfg.EnableWebTools)); err != nil {
		return nil, err
	}
	if err := host.Load(plugin.NewProviderPlugin(client)); err != nil {
		return nil, err
	}

	// Fallback model (Claude's --fallback-model): built lazily, used only if
	// the primary model's stream fails.
	var fallback *llm.Client
	if cfg.FallbackModel != "" {
		baseURL, apiKey := cfg.EndpointFor(cfg.FallbackModel)
		if f, ferr := llm.NewClient(llm.Config{
			BaseURL: baseURL, APIKey: apiKey, Model: cfg.FallbackModel,
			Timeout: 10 * time.Minute, Debug: cfg.Debug,
		}); ferr == nil {
			fallback = f
		}
	}

	// Custom external tools (config "tools"): register under a "custom" scope
	// and remember their permission overrides.
	customPerms := map[string]string{}
	for _, spec := range cfg.Tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		registry.RegisterIn("custom", tools.NewCommandTool(name, spec.Description, spec.Command, spec.InputSchema))
		customPerms[name] = strings.ToLower(spec.Permission)
	}

	sid := newSessionID()
	if cfg.SessionID != "" {
		sid = cfg.SessionID
	}

	a := &Agent{
		cfg:            cfg,
		client:         client,
		registry:       registry,
		host:           host,
		evbus:          bus,
		models:         ectx.Models,
		gohooks:        ectx.Hooks,
		sessionReg:     ectx.Session,
		mcp:            mcp.NewManager(),
		perms:          cfg.PermManager(),
		sessionID:      sid,
		createdAt:      time.Now(),
		events:         evCh,
		ctrl:           ctrl,
		customPerms:    customPerms,
		deferTools:     map[string]bool{},
		discovered:     map[string]bool{},
		sessionStartAt: time.Now(),
		fallbackClient: fallback,
	}
	a.hooks = hooks.NewManager(cfg.Hooks, hooks.Options{SessionID: a.sessionID, Workspace: cfg.Workspace, Mode: cfg.PermissionMode})
	a.sandbox = cfg.Sandbox()
	a.hooks.SetTranscript(cfg.SessionDir)
	go a.hooks.SessionStart(context.Background())

	// MCP: launch configured servers and register their tools into the "mcp"
	// scope. Failures are logged and skipped; the agent keeps running.
	a.mcp.Start(context.Background(), cfg.MCPServers)
	a.mcp.RegisterTools(registry)

	// ToolSearch lets the model discover deferred tools on demand (Codex /
	// Claude Code's tool search). Config custom tools and MCP tools are
	// deferred: their schemas are not injected inline.
	registry.RegisterIn("builtin", &toolSearchTool{ag: a})
	registry.RegisterIn("builtin", &enterPlanModeTool{ag: a})
	registry.RegisterIn("builtin", &exitPlanModeTool{ag: a})
	for _, spec := range cfg.Tools {
		if name := strings.TrimSpace(spec.Name); name != "" {
			a.deferTools[name] = true
		}
	}
	for _, names := range a.mcp.ToolNames() {
		for _, n := range names {
			a.deferTools[n] = true
		}
	}

	// Skills: user skills (~/.ccdp/skills) plus project skills (.ccdp/skills).
	a.skills = skills.NewStore()
	home, _ := os.UserHomeDir()
	a.skills.Load(filepath.Join(home, ".ccdp", "skills"),
		filepath.Join(cfg.Workspace, ".ccdp", "skills"))

	a.checkpoints = checkpoint.NewStore(cfg.SessionDir, a.sessionID)

	a.openTrace()

	a.emitSessionLifecycle(false)
	return a, nil
}

// CreateCheckpoint snapshots the workspace under a git-based checkpoint and
// returns its id. Non-git workspaces return an error.
func (a *Agent) CreateCheckpoint(summary string) (string, error) {
	id, err := a.checkpoints.Create(a.cfg.Workspace, summary)
	if err != nil {
		return "", err
	}
	if id == "" {
		a.emitStatus("no changes to checkpoint")
		return "", nil
	}
	a.emitStatus("checkpoint %s created", id)
	return id, nil
}

// RestoreCheckpoint rewinds the working tree to a checkpoint snapshot.
func (a *Agent) RestoreCheckpoint(id string) error {
	if err := a.checkpoints.Restore(a.cfg.Workspace, id); err != nil {
		return err
	}
	workspace.Invalidate(a.cfg.Workspace)
	a.emitStatus("restored checkpoint %s", id)
	return nil
}

// CheckpointList returns the session's checkpoints, newest first.
func (a *Agent) CheckpointList() []checkpoint.Record { return a.checkpoints.List() }

// AddDirectory grants the sandbox access to an extra directory at runtime.
func (a *Agent) AddDirectory(dir string) {
	a.sandbox.AddDir(dir)
	a.emitStatus("additional directory: %s", dir)
}

// AddDisallowedDirectory blocks a directory at runtime.
func (a *Agent) AddDisallowedDirectory(dir string) {
	a.sandbox.AddDisallowedDir(dir)
	a.emitStatus("disallowed directory: %s", dir)
}

// SkillNames returns the loaded skill names (used by /skills).
func (a *Agent) SkillNames() []string {
	all := a.skills.All()
	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.Name)
	}
	return names
}

// Resume restores an agent from a previously saved session snapshot.
func Resume(cfg *config.Config, snap *SessionSnapshot, evCh chan Event, ctrl chan Control) (*Agent, error) {
	a, err := New(cfg, evCh, ctrl)
	if err != nil {
		return nil, err
	}
	a.sessionID = snap.ID
	a.createdAt = snap.CreatedAt
	a.history = snap.History
	// The trace file was opened with the generated id in New; reopen it under
	// the resumed session's id, then re-discover deferred tools.
	a.closeTrace()
	a.openTrace()
	a.restoreDiscoveredFromTrace()
	a.emitSessionLifecycle(true)
	return a, nil
}

// emitSessionLifecycle publishes session events to the bus and plugins.
func (a *Agent) emitSessionLifecycle(resumed bool) {
	if resumed {
		a.sessionReg.RunResume(a.sessionID)
		a.evbus.Emit(events.TopicSessionResumed, events.SessionEvent{ID: a.sessionID})
		return
	}
	a.sessionReg.RunCreate(a.sessionID, a.cfg.Workspace)
	a.evbus.Emit(events.TopicSessionCreated, events.SessionEvent{ID: a.sessionID, Workspace: a.cfg.Workspace})
}

// Close tears down the plugin host, deinitializing plugins in reverse order.
// The agent is unusable afterwards.
func (a *Agent) Close() {
	a.sessionReg.RunDispose(a.sessionID)
	a.evbus.Emit(events.TopicSessionDisposed, events.SessionEvent{ID: a.sessionID})
	// Lifecycle hooks: SessionEnd then Stop, matching Claude Code's order.
	a.hooks.SessionEnd(context.Background())
	a.hooks.Stop(context.Background())
	a.mcp.Close()
	a.closeTrace()
	// Kill any background processes (ProcessStart) started this session so
	// REPLs and dev servers never outlive the agent.
	tools.ProcessStopAll()
	_ = a.host.Close()
}

// PluginNames returns the loaded plugin names in load order.
func (a *Agent) PluginNames() []string { return a.host.Names() }

// ProviderNames returns the registered LLM provider names.
func (a *Agent) ProviderNames() []string { return a.models.Names() }

// MCPServers returns the connected MCP server names.
func (a *Agent) MCPServers() []string { return a.mcp.Names() }

// MCPInfo returns connected MCP server name → advertised tool names.
func (a *Agent) MCPInfo() map[string][]string { return a.mcp.ToolNames() }

// MCPResources returns MCP server name → advertised resource URIs.
func (a *Agent) MCPResources() map[string][]string {
	out := map[string][]string{}
	ctx := context.Background()
	for _, name := range a.mcp.Names() {
		c, ok := a.mcp.Server(name)
		if !ok {
			continue
		}
		rs, err := c.Resources(ctx)
		if err != nil {
			continue
		}
		for _, r := range rs {
			out[name] = append(out[name], r.URI)
		}
	}
	return out
}

// MCPPrompts returns MCP server name → advertised prompt names.
func (a *Agent) MCPPrompts() map[string][]string {
	out := map[string][]string{}
	ctx := context.Background()
	for _, name := range a.mcp.Names() {
		c, ok := a.mcp.Server(name)
		if !ok {
			continue
		}
		ps, err := c.Prompts(ctx)
		if err != nil {
			continue
		}
		for _, p := range ps {
			out[name] = append(out[name], p.Name)
		}
	}
	return out
}

// Events returns the outgoing event channel.
func (a *Agent) Events() <-chan Event { return a.events }

// Controls returns the incoming control channel.
func (a *Agent) Controls() chan<- Control { return a.ctrl }

// PermissionMode returns the current permission mode.
func (a *Agent) PermissionMode() permissions.Mode { return a.perms.CurrentMode() }

// SetPermissionMode switches the mode and notifies the UI. Choosing "plan"
// also activates plan mode (Claude Code's plan permission mode); leaving it
// deactivates plan mode.
func (a *Agent) SetPermissionMode(mode permissions.Mode) {
	a.perms.SetMode(mode)
	a.mu.Lock()
	a.planMode = mode == permissions.ModePlan
	a.mu.Unlock()
	a.emit(Event{Type: EventModeChanged, Mode: mode})
	a.emitStatus("permission mode → %s", mode)
}

// SandboxMode returns the active sandbox mode.
func (a *Agent) SandboxMode() sandbox.Mode {
	return a.sandbox.Mode
}

// SetSandboxMode switches the sandbox mode at runtime.
func (a *Agent) SetSandboxMode(mode sandbox.Mode) {
	a.sandbox.Mode = mode
	a.cfg.SandboxMode = string(mode)
	a.emit(Event{Type: EventSandboxChanged})
	a.emitStatus("sandbox mode → %s", mode)
}

// Usage returns a snapshot of the session's token/cost accounting.
func (a *Agent) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// ContextUsage reports the estimated context consumption and the configured
// window, for the statusline "context" item (Claude Code shows the percent
// left until auto-compact).
func (a *Agent) ContextUsage() (used, window int) {
	used = a.estimateTokens()
	a.mu.Lock()
	window = a.cfg.ContextWindow
	a.mu.Unlock()
	if window <= 0 {
		window = 1
	}
	return used, window
}

// HooksList returns the configured hooks grouped by event.
func (a *Agent) HooksList() map[string][]string { return a.hooks.List() }

// PlanMode reports whether plan mode is active.
func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planMode
}

// SetPlanMode toggles plan mode. While active, the model proposes a plan and
// mutating tools are blocked until the user approves it.
func (a *Agent) SetPlanMode(on bool) {
	a.mu.Lock()
	a.planMode = on
	a.mu.Unlock()
	a.emit(Event{Type: EventPlanModeChanged, PlanMode: on})
	if on {
		a.emitStatus("plan mode on — model proposes a plan before any execution")
	} else {
		a.emitStatus("plan mode off")
	}
}

// SessionID returns the session identifier.
func (a *Agent) SessionID() string { return a.sessionID }

// HasHistory reports whether the conversation contains any messages.
func (a *Agent) HasHistory() bool { return len(a.history) > 0 }

// Model returns the configured model name.
func (a *Agent) Model() string { return a.cfg.Model }

// WorkspaceLabel returns the absolute workspace path.
func (a *Agent) WorkspaceLabel() string { return a.cfg.Workspace }

// SessionDir returns the directory where sessions are persisted.
func (a *Agent) SessionDir() string { return a.cfg.SessionDir }

// SetModel switches the model by resolving its provider, rebuilding the LLM
// client and re-routing the provider through the ModelRegistry.
func (a *Agent) SetModel(model string) {
	if model == "" {
		return
	}
	baseURL, apiKey := a.cfg.EndpointFor(model)
	client, err := llm.NewClient(llm.Config{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		Timeout: 10 * time.Minute,
		Debug:   a.cfg.Debug,
	})
	if err != nil {
		a.emitStatus("failed to switch model: %v", err)
		return
	}
	a.cfg.Model = model
	a.client = client
	a.models.Register(client)
	a.models.Route(model, client.Name())
	a.mu.Lock()
	a.modelSwitchMsg = "# Model\nYou have been switched to the model \"" + model + "\". Adjust your responses to its capabilities."
	a.mu.Unlock()
	a.emitStatus("model switched to %s", model)
}

// emit sends an event to the UI. Status events are dropped when the buffer is
// full; everything else blocks so no user-visible message is lost. Every event
// is also appended to the session trace.
func (a *Agent) emit(ev Event) {
	a.writeTrace(ev)
	if ev.Type == EventStatus {
		select {
		case a.events <- ev:
		default:
		}
		return
	}
	a.events <- ev
}

// emitStatus is a convenience wrapper.
func (a *Agent) emitStatus(format string, args ...any) {
	a.emit(Event{Type: EventStatus, Text: fmt.Sprintf(format, args...)})
}

// logv prints a diagnostic line to stderr when --verbose is set.
func (a *Agent) logv(format string, args ...any) {
	if a.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "ccdp: "+format+"\n", args...)
	}
}

// Run is the agent's main control loop. It is the sole consumer of the control
// channel and must run in its own goroutine.
func (a *Agent) Run() {
	for {
		select {
		case c := <-a.ctrl:
			a.handle(c)
		}
	}
}

func (a *Agent) handle(c Control) {
	switch c.Type {
	case ControlUserMessage:
		a.mu.Lock()
		busy := a.busy
		a.mu.Unlock()
		if busy {
			a.mu.Lock()
			a.pendingMsgs = append(a.pendingMsgs, c.Text)
			a.mu.Unlock()
			a.emitStatus("queued message (agent is busy; press ctrl+c to interrupt)")
			return
		}
		a.startTurn(c.Text)
	case ControlApproval:
		a.mu.Lock()
		resp := a.approvalResp
		a.mu.Unlock()
		if resp != nil {
			resp <- approvalAnswer{approve: c.Approve, remember: c.Remember}
		}
	case ControlPlanResp:
		a.mu.Lock()
		resp := a.planResp
		a.mu.Unlock()
		if resp != nil {
			resp <- c.PlanApprove
		}
	case ControlSetPlan:
		a.SetPlanMode(c.PlanOn)
	case ControlInterrupt:
		a.interrupt()
	case ControlStop:
		a.mu.Lock()
		a.stop = true
		cancel := a.turnCancel
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		a.emitStatus("stop requested")
	case ControlClearHistory:
		a.clearHistory()
	case ControlCompactNow:
		a.mu.Lock()
		busy := a.busy
		a.mu.Unlock()
		if busy {
			a.emitStatus("cannot compact while a turn is running")
			return
		}
		a.compact()
	case ControlSetMode:
		a.SetPermissionMode(c.Mode)
	case ControlSetSandbox:
		a.SetSandboxMode(c.SandboxMode)
	case ControlRemove:
		a.removeLast(c.Count)
	case ControlRewind:
		a.rewindTo(c.Count)
	default:
		a.emitStatus("unhandled control %d", c.Type)
	}
}

// startTurn launches a turn goroutine.
func (a *Agent) startTurn(text string) {
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	go a.runTurn(text)
}

// runTurn processes one user message through the full agent loop:
// Infer → ToolDispatch → ApprovalGate → Compact.
func (a *Agent) runTurn(text string) {
	defer a.turnFinished()

	a.mu.Lock()
	a.interruptFlag = false
	a.stop = false
	a.mu.Unlock()
	a.resetOutputBudget()
	a.resetGuardianBudget()
	// Fallback budget resets per turn: one fallback attempt per turn, not per
	// session (a transient primary failure should not disable the primary for
	// the rest of the session).
	a.mu.Lock()
	a.usedFallback = false
	a.activeModel = a.cfg.Model
	a.mu.Unlock()

	// AutoMem: reset the per-turn tracking for this turn.
	a.mu.Lock()
	a.turnUserMsg = text
	a.turnTouched = nil
	a.turnSummary = ""
	a.mu.Unlock()

	// UserPromptSubmit hook can veto the message before any work happens.
	if ho := a.hooks.UserPromptSubmit(context.Background(), text); ho.Decision == hooks.DecisionBlock {
		reason := ho.Reason
		if reason == "" {
			reason = "blocked by UserPromptSubmit hook"
		}
		a.emit(Event{Type: EventUserMsg, Text: text})
		a.emit(Event{Type: EventError, Text: reason})
		a.emit(Event{Type: EventTurnDone})
		return
	} else if ho.AdditionalContext != "" {
		// Hook-provided context rides along as a system message.
		a.appendHistory(messages.Message{
			Role: messages.RoleSystem, Content: ho.AdditionalContext, CreatedAt: time.Now(),
		})
	}

	a.emit(Event{Type: EventUserMsg, Text: text})
	a.appendHistory(messages.Message{
		Role: messages.RoleUser, Content: text, CreatedAt: time.Now(),
	})

	steps := 0         // model-call iterations in this turn (--max-turns)
	continueCount := 0 // automatic continuation for token-limited replies
	for {
		if a.interrupted() {
			// Flag the interruption so the next request tells the model that
			// tools may have partially executed (Codex's INTERRUPTED_GUIDANCE).
			a.mu.Lock()
			a.interruptNote = true
			a.mu.Unlock()
			a.emitStatus("turn interrupted")
			a.emit(Event{Type: EventTurnDone})
			return
		}
		// Compaction gate (Codex: compact between turns).
		if a.needsCompact() {
			a.emitStatus("context near limit, compacting…")
			a.compact()
			if a.interrupted() {
				a.emit(Event{Type: EventTurnDone})
				return
			}
		}

		// -- Infer --
		// The turn context lives for the whole turn: the LLM stream and every
		// tool invocation share it, so an interrupt aborts all of them.
		turnCtx, turnCancel := context.WithCancel(context.Background())
		defer turnCancel()
		a.mu.Lock()
		a.turnCtx = turnCtx
		a.turnCancel = turnCancel
		a.mu.Unlock()

		// --max-turns: stop calling the model after the cap; report the
		// accumulated state so far instead of silently hanging.
		if a.cfg.MaxTurns > 0 && steps >= a.cfg.MaxTurns {
			a.emitStatus("max turns reached (%d) — stopping", a.cfg.MaxTurns)
			a.emit(Event{Type: EventTurnDone})
			return
		}
		// --max-budget: hard spending cap; stop when the session cost exceeds it.
		if a.cfg.MaxBudgetUSD > 0 && a.Usage().Cost >= a.cfg.MaxBudgetUSD {
			a.emitStatus("max budget reached ($%.2f of $%.2f) — stopping", a.Usage().Cost, a.cfg.MaxBudgetUSD)
			a.emit(Event{Type: EventTurnDone})
			return
		}
		steps++

		a.emitStatus("thinking…")
		req := a.buildRequest()

		var streamed strings.Builder
		var res llm.StreamResult
		var streamErr error
		streamDone := make(chan struct{})
		go func() {
			r, err := a.client.StreamWithReasoning(turnCtx, req, func(delta string) {
				streamed.WriteString(delta)
				a.emit(Event{Type: EventStream, Text: delta})
			}, func(delta string) {
				a.emit(Event{Type: EventReasoning, Text: delta})
			})
			res = r
			streamErr = err
			close(streamDone)
		}()
		<-streamDone

		if streamErr != nil {
			if a.interrupted() {
				a.mu.Lock()
				a.interruptNote = true
				a.mu.Unlock()
				a.emitStatus("stream interrupted")
				a.emit(Event{Type: EventTurnDone})
				return
			}
			// Fallback model (Claude's --fallback-model): one retry on the
			// backup client, then give up. activeModel follows so usage is
			// priced with the fallback rate card.
			if !a.usedFallback && a.fallbackClient != nil {
				a.usedFallback = true
				a.client = a.fallbackClient
				a.mu.Lock()
				a.activeModel = a.cfg.FallbackModel
				a.mu.Unlock()
				a.emitStatus("primary model failed (%v) — falling back to %s", streamErr, a.cfg.FallbackModel)
				continue
			}
			a.emit(Event{Type: EventError, Text: "LLM error: " + streamErr.Error()})
			a.emit(Event{Type: EventTurnDone})
			return
		}

		// Accumulate token/cost usage and surface it to the UI.
		if res.PromptTokens > 0 || res.CompletionTok > 0 {
			a.recordUsage(res.PromptTokens, res.CompletionTok, res.CachedTokens)
		}

		callText := streamed.String()
		var calls []messages.ToolCall
		for _, rc := range res.ToolCalls {
			calls = append(calls, messages.ToolCall{
				ID:        rc.ID,
				Name:      rc.Function.Name,
				Arguments: llm.UnmarshalArgs(rc.Function.Arguments.String()),
			})
		}
		a.appendHistory(messages.AssistantWithTools(callText, calls))

		// No tool calls → the assistant answered; turn is done, unless plan
		// mode is on (then the text is a plan awaiting approval), or the reply
		// was cut off by the token limit (then we continue automatically).
		if len(calls) == 0 {
			if a.inPlanMode() {
				if !a.requestPlanApproval(callText) {
					a.emit(Event{Type: EventStatus, Text: "plan not approved — no changes made"})
					a.emit(Event{Type: EventTurnDone})
					return
				}
				// Approved: exit plan mode and execute the plan step by step.
				a.setPlanMode(false)
				a.appendHistory(messages.Message{
					Role: messages.RoleUser, Content: "The plan above is approved. Execute it now, following the plan step by step. Verify each step.",
					CreatedAt: time.Now(),
				})
				continue
			}
			// Claude-style continuation: the reply hit MaxReplyTokens; ask the
			// model to keep going (bounded, so it cannot loop forever).
			if res.FinishReason == "length" && a.cfg.MaxReplyTokens > 0 && continueCount < 3 {
				continueCount++
				a.emitStatus("reply hit the token limit — continuing…")
				a.appendHistory(messages.Message{
					Role:      messages.RoleUser,
					Content:   "Continue your reply from where you left off.",
					CreatedAt: time.Now(),
				})
				continue
			}
			// AutoMem: remember the final assistant answer as the turn's outcome.
			a.mu.Lock()
			a.turnSummary = callText
			a.mu.Unlock()
			a.emit(Event{Type: EventTurnDone})
			return
		}

		// -- ToolDispatch + ApprovalGate --
		// Parallel dispatch (Codex-style) when configured; results are always
		// appended in call order so the conversation stays deterministic.
		results := a.dispatchTools(calls)
		// Every tool_call id MUST get a result message or the next request
		// violates the protocol (providers answer 400). dispatchTools fills
		// synthetic results for calls skipped by an interrupt, so we append
		// all of them unconditionally — never break early here.
		for i, r := range results {
			a.appendHistory(messages.NewToolResult(calls[i], r.output, r.isErr))
		}
		// Loop back to Infer with the tool results appended.
	}
}

// turnFinished runs when a turn completes: persists the session, flushes the
// busy flag and drains any queued user messages. The busy flag is cleared only
// after saving so no new turn can mutate history concurrently with Save.
func (a *Agent) turnFinished() {
	a.mu.Lock()
	a.turnCancel = nil
	a.mu.Unlock()
	// AutoMem: fold this turn's request / touched files / outcome into the
	// session memory log before any queued turn resets the tracking fields.
	a.recordMemory()
	_ = a.Save()

	a.mu.Lock()
	a.busy = false
	if len(a.pendingMsgs) > 0 {
		next := a.pendingMsgs[0]
		a.pendingMsgs = a.pendingMsgs[1:]
		a.busy = true
		a.mu.Unlock()
		go a.runTurn(next)
		return
	}
	a.mu.Unlock()
}

// appendHistory appends messages to the conversation under the lock and
// publishes a domain event so plugins can observe the conversation.
func (a *Agent) appendHistory(msgs ...messages.Message) {
	a.mu.Lock()
	a.history = append(a.history, msgs...)
	a.mu.Unlock()
	for _, msg := range msgs {
		a.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(msg.Role), Content: msg.Content})
	}
}

// dispatchTools executes a batch of tool calls. With maxParallelTools > 1 the
// calls run concurrently (bounded worker pool); approvals stay serialized so
// the user only ever sees one modal. Results come back in call order.
func (a *Agent) dispatchTools(calls []messages.ToolCall) []toolRunResult {
	n := a.cfg.MaxParallelTools
	if n < 1 || len(calls) <= 1 {
		results := make([]toolRunResult, len(calls))
		for i, tc := range calls {
			if a.interrupted() {
				results[i] = toolRunResult{output: "interrupted before execution", isErr: true}
				continue
			}
			results[i].output, results[i].isErr = a.executeTool(tc)
		}
		return results
	}
	if n > len(calls) {
		n = len(calls)
	}

	jobs := make(chan int)
	results := make([]toolRunResult, len(calls))
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if a.interrupted() {
					results[idx] = toolRunResult{output: "interrupted before execution", isErr: true}
					continue
				}
				out, err := a.executeTool(calls[idx])
				results[idx] = toolRunResult{output: out, isErr: err}
			}
		}()
	}
	for i := range calls {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

type toolRunResult struct {
	output string
	isErr  bool
}

// recordUsage accumulates token usage and cost for the session, then publishes
// the snapshot on the domain bus (TopicUsageUpdated). cachedTok counts prompt
// tokens served from a provider cache (OpenAI prompt_tokens_details). Cost is
// priced with the ACTIVE model, which may be the fallback after a switch.
func (a *Agent) recordUsage(inputTok, outputTok, cachedTok int) {
	a.mu.Lock()
	a.usage.InputTokens += inputTok
	a.usage.OutputTokens += outputTok
	a.usage.CachedTokens += cachedTok
	a.usage.TurnCount++
	model := a.activeModel
	if model == "" {
		model = a.cfg.Model
	}
	a.usage.Cost = a.cfg.CostFor(model, a.usage.InputTokens, a.usage.OutputTokens)
	u := a.usage
	// Anchor the compaction estimate: this usage was reported for a request
	// built from the current history length (the assistant reply and tool
	// results are appended after this point).
	a.tokenBaseline.promptTokens = inputTok
	a.tokenBaseline.historyLen = len(a.history)
	a.mu.Unlock()
	ev := Event{Type: EventUsage, Usage: &u}
	a.events <- ev
	a.evbus.Emit(events.TopicUsageUpdated, events.UsageEvent{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TurnCount:    u.TurnCount,
		Cost:         u.Cost,
	})
}

// executeTool runs one tool call, handling Go hooks, shell hooks, permission
// and approval.
func (a *Agent) executeTool(tc messages.ToolCall) (string, bool) {
	// Plan mode hard-blocks mutating tools even if the model somehow calls one.
	if a.inPlanMode() && !planReadOnlyTools[tc.Name] {
		a.emit(toolEvent(tc, "denied", "blocked in plan mode"))
		return fmt.Sprintf("blocked in plan mode: %s modifies state; only read-only tools are available until the plan is approved", tc.Name), true
	}

	tool, ok := a.registry.Get(tc.Name)
	if !ok {
		a.emit(toolEvent(tc, "error", "unknown tool "+tc.Name))
		return fmt.Sprintf("Error: unknown tool %q", tc.Name), true
	}

	// Sandbox × approval linkage: in strict sandbox mode, network tools are
	// blocked unless network access was explicitly allowed (Codex's
	// sandbox_mode ↔ approval matrix).
	if a.sandbox != nil && a.sandbox.Mode == sandbox.ModeStrict && !a.sandbox.AllowNetwork {
		switch tc.Name {
		case "WebFetch", "WebSearch":
			a.emit(toolEvent(tc, "denied", "network blocked by strict sandbox"))
			return fmt.Sprintf("permission denied: %s needs network access, which strict sandbox mode blocks (set sandbox_allow_network: true to permit)", tc.Name), true
		}
	}

	// Custom tools may carry a config-level permission override that bypasses
	// the normal approval gate ("allow" runs it, "deny" refuses it).
	if perm, isCustom := a.customPerms[tc.Name]; isCustom {
		switch perm {
		case "allow":
			return a.runTool(tc, tool)
		case "deny":
			a.emit(toolEvent(tc, "denied", "denied by tool config"))
			return fmt.Sprintf("permission denied: %s is configured with permission \"deny\"", tc.Name), true
		}
	}

	// In-process Go hooks run first; their verdict can veto the call.
	if dec, reason := a.gohooks.RunPreTool(tc.Name, tc.Arguments); dec != plugin.DecisionNone {
		switch dec {
		case plugin.DecisionDeny:
			if reason == "" {
				reason = "blocked by Go pre-tool hook"
			}
			a.emit(toolEvent(tc, "denied", reason))
			return fmt.Sprintf("blocked by hook: %s", reason), true
		case plugin.DecisionAsk:
			approved, remember := a.requestApproval(tc, "Go hook requests approval")
			if !approved {
				a.emit(toolEvent(tc, "denied", reason))
				return fmt.Sprintf("permission denied: %s", reason), true
			}
			if remember {
				a.perms.RememberAllow(permissions.SessionKey(tc.Name, tc.Arguments))
			}
		case plugin.DecisionAllow:
			return a.runTool(tc, tool)
		}
	}

	// PreToolUse hooks can allow, deny or escalate a call.
	if ho := a.hooks.PreToolUse(a.turnCtx, tc.Name, tc.Arguments); ho.Decision != hooks.DecisionNone {
		switch ho.Decision {
		case hooks.DecisionDeny, hooks.DecisionBlock:
			reason := ho.Reason
			if reason == "" {
				reason = "blocked by PreToolUse hook"
			}
			a.emit(toolEvent(tc, "denied", reason))
			return fmt.Sprintf("blocked by hook: %s", reason), true
		case hooks.DecisionAsk:
			approved, remember := a.requestApproval(tc, "PreToolUse hook requests approval")
			if !approved {
				reason := "denied by user after hook request"
				if ho.Reason != "" {
					reason = ho.Reason
				}
				a.emit(toolEvent(tc, "denied", reason))
				return fmt.Sprintf("permission denied: %s", reason), true
			}
			if remember {
				a.perms.RememberAllow(permissions.SessionKey(tc.Name, tc.Arguments))
			}
		case hooks.DecisionAllow:
			// Hook already approved; skip the permission gate.
			return a.runTool(tc, tool)
		}
	}

	decision, reason := a.perms.Check(tc.Name, tc.Arguments)
	switch decision {
	case permissions.DecisionDeny:
		a.emit(toolEvent(tc, "denied", ""))
		return fmt.Sprintf("permission denied: %s", reason), true
	case permissions.DecisionAsk:
		approved, remember := a.requestApproval(tc, reason)
		if remember {
			key := permissions.SessionKey(tc.Name, tc.Arguments)
			if approved {
				a.perms.RememberAllow(key)
			} else {
				a.perms.RememberDeny(key)
			}
		}
		if !approved {
			a.emit(toolEvent(tc, "denied", ""))
			return fmt.Sprintf("permission denied by user: %s", reason), true
		}
	}

	return a.runTool(tc, tool)
}

// runTool executes an approved tool call, publishing the tool pipeline domain
// events and running PostToolUse + Go post hooks.
func (a *Agent) runTool(tc messages.ToolCall, tool tools.Tool) (string, bool) {
	// Guardian review (Codex's guardian): high-risk calls are reviewed by a
	// read-only sub-agent before executing. Enabled via enable_guardian.
	if a.cfg.EnableGuardian && guardianRisk(tc.Name) {
		if err := a.guardianCheck(tc); err != nil {
			a.emit(toolEvent(tc, "denied", err.Error()))
			return err.Error(), true
		}
	}
	a.evbus.Emit(events.TopicToolPreExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "running"})
	a.emit(Event{Type: EventToolStart, Tool: &ToolEvent{ID: tc.ID, Name: tc.Name, Args: tc.Arguments}})
	a.emit(toolEvent(tc, "running", ""))
	a.emitStatus("%s running…", tc.Name)
	start := time.Now()
	tctx := &tools.Context{
		Context:    a.turnCtx,
		WorkingDir: a.cfg.Workspace,
		SessionDir: a.cfg.SessionDir,
		Args:       tc.Arguments,
		Timeout:    a.cfg.BashTimeout(),
		Sandbox:    a.sandbox,
		Subagent:   a.runSubagent,
		Subagents:  a.runSubagents,
		Skills:     a.skills,
		Notify: func(line string) {
			a.emit(Event{Type: EventToolStream, Tool: &ToolEvent{
				ID: tc.ID, Name: tc.Name, Status: "stream", Output: line,
			}})
		},
	}
	out, err := tool.Run(tctx)
	elapsed := time.Since(start).Round(time.Millisecond)
	status := errString(err)
	if status == "" {
		status = "ok"
	}
	a.logv("tool %s %s in %s", tc.Name, status, elapsed)

	// Invalidate the workspace model after anything that may have changed files.
	switch tc.Name {
	case "Write", "Edit", "Bash":
		workspace.Invalidate(a.cfg.Workspace)
	}

	// AutoMem: remember files the turn modified so the memory log can say what
	// changed. Only Write/Edit carry a reliable file_path argument.
	if a.cfg.EnableMemory {
		switch tc.Name {
		case "Write", "Edit":
			if p, _ := tc.Arguments["file_path"].(string); p != "" {
				a.recordTouched(p)
			}
		}
	}

	if err != nil {
		a.emit(toolEvent(tc, "error", err.Error()))
		a.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "error", Error: err.Error()})
		// PostToolUseFailure hooks observe the failure (non-blocking).
		a.hooks.PostToolUseFailure(a.turnCtx, tc.Name, tc.Arguments, err.Error())
		return fmt.Sprintf("%s failed after %s: %v", tc.Name, elapsed, err), true
	}
	// Large results are persisted to disk with a preview + path so the model
	// can re-read them (Claude Code's toolResultStorage idea).
	out = a.maybePersistResult(tc, out)
	// Per-turn aggregate budget: beyond the cap, results are clipped hard.
	out = a.clipAggregate(out)

	// PostToolUse shell hooks can append context to the result.
	if ho := a.hooks.PostToolUse(a.turnCtx, tc.Name, tc.Arguments, out); ho.HookSpecificOutput != "" {
		out += "\n" + ho.HookSpecificOutput
	}
	// In-process Go post hooks run alongside.
	if extra := a.gohooks.RunPostTool(tc.Name, tc.Arguments, out); extra != "" {
		out += extra
	}

	a.emit(toolEvent(tc, "success", out))
	a.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	a.evbus.Emit(events.TopicToolResult, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	return out, false
}

// requestApproval blocks until the user answers the approval request. It is
// safe for concurrent tool workers: approvalMu serializes the whole
// request-answer cycle so only one modal is ever pending (Codex's single
// approval slot).
func (a *Agent) requestApproval(tc messages.ToolCall, reason string) (bool, bool) {
	a.approvalMu.Lock()
	defer a.approvalMu.Unlock()

	req := &ApprovalRequest{
		ID:      tc.ID,
		Tool:    tc.Name,
		Command: prettyArgs(tc.Arguments),
		Reason:  reason,
	}
	a.mu.Lock()
	a.pendingApproval = req
	resp := make(chan approvalAnswer, 1)
	a.approvalResp = resp
	a.mu.Unlock()

	a.emit(Event{Type: EventApproval, Approval: req})

	select {
	case ans := <-resp:
		return ans.approve, ans.remember
	case <-a.turnCtx.Done():
		a.emitStatus("approval skipped (interrupted)")
		return false, false
	}
}

// inPlanMode reports whether plan mode is active (guarded).
func (a *Agent) inPlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planMode
}

// setPlanMode flips plan mode without emitting a status (used mid-turn after
// plan approval).
func (a *Agent) setPlanMode(on bool) {
	a.mu.Lock()
	a.planMode = on
	a.mu.Unlock()
}

// requestPlanApproval surfaces the proposed plan and blocks until the user
// approves or rejects it. Returns true to execute.
func (a *Agent) requestPlanApproval(plan string) bool {
	req := &PlanRequest{ID: fmt.Sprintf("plan-%d", time.Now().UnixNano()), Plan: plan}
	a.mu.Lock()
	resp := make(chan bool, 1)
	a.planResp = resp
	a.mu.Unlock()

	a.emit(Event{Type: EventPlan, Plan: req})
	a.emitStatus("plan ready — approve (y) to execute, deny (n) to reject")

	select {
	case ok := <-resp:
		return ok
	case <-a.turnCtx.Done():
		a.emitStatus("plan approval skipped (interrupted)")
		return false
	}
}

// interrupted reports whether the current turn was interrupted or stopped.
func (a *Agent) interrupted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interruptFlag || a.stop
}

// interrupt cancels the in-flight LLM call and tool execution.
func (a *Agent) interrupt() {
	a.mu.Lock()
	a.interruptFlag = true
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// clearHistory wipes the conversation (keeps the system message).
func (a *Agent) clearHistory() {
	a.mu.Lock()
	a.history = a.history[:0]
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryCleared})
	a.emitStatus("history cleared")
	_ = a.Save()
}

// removeLast drops the trailing n messages from the conversation.
func (a *Agent) removeLast(n int) {
	a.mu.Lock()
	busy := a.busy
	if !busy && n > 0 && len(a.history) > 0 {
		if n > len(a.history) {
			n = len(a.history)
		}
		a.history = a.history[:len(a.history)-n]
	}
	a.mu.Unlock()
	if busy {
		a.emitStatus("cannot remove messages while a turn is running")
		return
	}
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("removed last %d message(s)", n)})
	_ = a.Save()
}

// rewindTo keeps only the first n messages, dropping everything after them.
func (a *Agent) rewindTo(n int) {
	a.mu.Lock()
	busy := a.busy
	if !busy {
		if n < 0 {
			n = 0
		}
		if n > len(a.history) {
			n = len(a.history)
		}
		a.history = a.history[:n]
	}
	a.mu.Unlock()
	if busy {
		a.emitStatus("cannot rewind while a turn is running")
		return
	}
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("rewound to message %d", n)})
	_ = a.Save()
}

// historyLen returns the number of messages in the conversation.
func (a *Agent) historyLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.history)
}

// prettyArgs renders tool arguments compactly for display.
func prettyArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		return cmd
	}
	if p, ok := args["file_path"].(string); ok {
		return p
	}
	return fmt.Sprintf("%v", args)
}

func toolEvent(tc messages.ToolCall, status, output string) Event {
	return Event{
		Type: EventToolResult,
		Tool: &ToolEvent{
			ID:     tc.ID,
			Name:   tc.Name,
			Args:   tc.Arguments,
			Status: status,
			Output: truncateResult(output, 2000),
		},
	}
}

func newSessionID() string {
	return fmt.Sprintf("%d%06d", time.Now().Unix(), time.Now().Nanosecond()/1000)
}
