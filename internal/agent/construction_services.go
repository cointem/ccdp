package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
)

// extensionHost aggregates the domain registries, plugin host, and the
// primary/fallback provider bindings produced before the Agent literal exists.
type extensionHost struct {
	bus      *events.Bus
	registry *tools.Registry
	models   *plugin.ModelRegistry
	ectx     *plugin.Context
	host     *plugin.Host
	// primaryBinding is always resolved; fallbackBinding is the zero binding when
	// no fallback model is configured or when it could not be resolved.
	primaryBinding  modelBinding
	fallbackBinding modelBinding
}

// buildExtensionHost wires the domain bus, tool registry, model registry and
// plugin host, and resolves the primary and fallback provider bindings. It
// takes the cancellation func of the yet-to-exist root context so a rejected
// construction can roll back without leaving a live root or plugin.
func buildExtensionHost(cfg *config.Config, opts Options, rootCancel context.CancelFunc) (*extensionHost, error) {
	bus := events.NewBus()
	registry := tools.NewRegistry()
	models := plugin.NewModelRegistry()
	if opts.ProviderRegistry != nil {
		models = opts.ProviderRegistry.Clone()
	}
	primaryBinding, resolveErr := providerBinding(*cfg, models, cfg.Model, 1)
	if resolveErr != nil {
		rootCancel()
		return nil, resolveErr
	}
	client := primaryBinding.client
	ectx := &plugin.Context{
		Events:  bus,
		Tools:   registry,
		Models:  models,
		Hooks:   plugin.NewGoHooks(),
		Session: plugin.NewSessionRegistry(),
	}
	host := plugin.NewHost(ectx)
	// New performs a few fallible extension steps before the Agent exists. Keep
	// one rollback path for those steps so a rejected construction never leaves
	// the root context or an already-loaded plugin alive.
	rollback := func() {
		rootCancel()
		_ = host.Close()
	}
	registry.SetChangeListener(func() {
		bus.Emit(events.TopicToolChange, events.ToolEvent{ToolName: ""})
	})
	if err := host.Load(plugin.NewToolsPlugin(cfg.WebToolsEnabled())); err != nil {
		rollback()
		return nil, err
	}
	if len(cfg.LanguageServers) == 0 {
		registry.Unregister("builtin", "CodeNavigate")
	}
	// A supplied registry may already contain an explicit fake/plugin route.
	// Loading the default HTTP plugin in that case would overwrite a route for
	// the same model, so only install the built-in plugin when this model was
	// resolved by the HTTP adapter.
	if _, suppliedRoute := opts.ProviderRegistry.ResolveRoute(cfg.Model); opts.ProviderRegistry == nil || !suppliedRoute {
		httpFingerprint := primaryBinding.httpBinding(cfg.ResolveProvider(cfg.Model).APIKey)
		if err := host.Load(plugin.NewHTTPProviderPlugin(client, cfg.Model, httpFingerprint)); err != nil {
			rollback()
			return nil, err
		}
	}

	// Fallback model (Claude's --fallback-model): built lazily, used only if
	// the primary model's stream fails.
	var fallbackBinding modelBinding
	if cfg.FallbackModel != "" {
		// An unresolvable fallback stays unavailable rather than failing
		// construction, matching the primary provider remaining usable.
		fallbackBinding, _ = providerBinding(*cfg, models, cfg.FallbackModel, 1)
	}
	return &extensionHost{
		bus:             bus,
		registry:        registry,
		models:          models,
		ectx:            ectx,
		host:            host,
		primaryBinding:  primaryBinding,
		fallbackBinding: fallbackBinding,
	}, nil
}

// startSessionServices launches the event worker and the sandbox/hooks/MCP
// stack. MCP startup and SessionStart run only after every configured server
// has completed its handshake, so a failed candidate never produces a visible
// hook side effect.
func (a *Agent) startSessionServices(rootCancel context.CancelFunc, isolated bool, inheritedCapabilities []protocol.CapabilityRequest, inheritedExecutionGuard *sandbox.ExecutionGuard) error {
	a.services.sandbox = a.services.cfg.Sandbox()
	a.services.sandbox.SetExecutionGuard(inheritedExecutionGuard)
	if a.services.resources != nil {
		// Execution caches and temporary files live for this session only. The
		// directory is created by the harness and removed when Resources.Close
		// shuts down the session; subprocess invocations never receive host /tmp.
		if scratch := a.services.resources.ScratchDir(); scratch != "" {
			a.services.sandbox.AddScratchDir(scratch)
		}
	}
	for _, capability := range inheritedCapabilities {
		if err := applyCapability(a.services.sandbox, capability); err != nil {
			return fmt.Errorf("inherit parent sandbox capability: %w", err)
		}
	}
	a.services.hooks = hooks.NewManager(a.services.cfg.Hooks, hooks.Options{SessionID: a.conversation.sessionID, Workspace: a.services.cfg.Workspace, Mode: a.services.cfg.PermissionMode, Sandbox: a.services.sandbox, FailClosed: true})
	a.services.mcp.SetSandbox(a.services.sandbox)
	a.services.hooks.SetTranscript(a.services.cfg.SessionDir)
	if !isolated {
		if err := a.services.mcp.StartChecked(a.lifecycle.rootCtx, a.services.cfg.MCPServers); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
		a.services.mcp.RegisterTools(a.services.registry)
		if hookOut := a.runHookWithJournal(a.lifecycle.rootCtx, hooks.EventSessionStart, func(hctx context.Context) hooks.Output {
			return a.services.hooks.SessionStart(hctx)
		}); hookOut.Decision == hooks.DecisionDeny || hookOut.Decision == hooks.DecisionBlock {
			if err := a.persistenceFailure(); err != nil {
				a.abortConstruction(rootCancel, true)
				return err
			}
		}
	}
	return nil
}

// finalizeToolsAndSkills registers the built-in control tools, records the
// deferred (MCP + config custom) tools for on-demand discovery, and loads user
// and project skills.
func (a *Agent) finalizeToolsAndSkills(isolated bool) {
	a.services.registry.RegisterIn("builtin", &toolSearchTool{ag: a})
	a.services.registry.RegisterIn("builtin", &askUserQuestionTool{ag: a})
	a.services.registry.RegisterIn("builtin", &enterPlanModeTool{ag: a})
	a.services.registry.RegisterIn("builtin", &exitPlanModeTool{ag: a})
	for _, spec := range a.services.cfg.Tools {
		if name := strings.TrimSpace(spec.Name); name != "" {
			a.discovery.deferTools[name] = true
		}
	}
	for _, names := range a.services.mcp.ToolNames() {
		for _, n := range names {
			a.discovery.deferTools[n] = true
		}
	}
	a.services.skills = skills.NewStore()
	if !isolated {
		home, _ := os.UserHomeDir()
		a.services.skills.Load(filepath.Join(home, ".ccdp", "skills"),
			filepath.Join(a.services.cfg.Workspace, ".ccdp", "skills"))
	}
}

// openTraceAndSupervisor opens the JSONL trace and attaches the session
// supervisor, then publishes the session-created lifecycle event.
func (a *Agent) openTraceAndSupervisor(opts Options) {
	a.openTrace()
	a.services.hooks.SetTranscript(a.TracePath())
	a.streamEpoch = nextRuntimeID("stream")
	if opts.Supervisor != nil {
		a.services.supervisor = opts.Supervisor
	} else {
		a.services.supervisor = newSessionSupervisor(a)
		a.services.supervisorOwner = true
		if err := a.reconcileDescendantUsage(); err != nil {
			a.emitStatus("token usage recovery incomplete: %v", err)
		}
	}
	a.emitSessionLifecycle(false)
}

// emitSessionLifecycle publishes session events to the bus and plugins.
func (a *Agent) emitSessionLifecycle(resumed bool) {
	if resumed {
		a.services.sessionReg.RunResume(a.conversation.sessionID)
		a.services.evbus.Emit(events.TopicSessionResumed, events.SessionEvent{ID: a.conversation.sessionID})
		return
	}
	a.services.sessionReg.RunCreate(a.conversation.sessionID, a.services.cfg.Workspace)
	a.services.evbus.Emit(events.TopicSessionCreated, events.SessionEvent{ID: a.conversation.sessionID, Workspace: a.services.cfg.Workspace})
}
