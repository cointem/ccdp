package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/codemode"
	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
)

// extensionRegistries aggregates the domain registries and the
// primary/fallback provider bindings produced before the Agent literal exists.
type extensionRegistries struct {
	bus      *events.Bus
	registry *tools.Registry
	models   *plugin.ModelRegistry
	// primaryBinding is always resolved; fallbackBinding is the zero binding when
	// no fallback model is configured or when it could not be resolved.
	primaryBinding  modelBinding
	fallbackBinding modelBinding
}

// buildExtensionRegistries registers built-ins and resolves provider bindings.
func buildExtensionRegistries(cfg *config.Config, opts Options, rootCancel context.CancelFunc) (*extensionRegistries, error) {
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
	registry.SetChangeListener(func() {
		bus.Emit(events.TopicToolChange, events.ToolEvent{ToolName: ""})
	})
	tools.RegisterBuiltins(registry, cfg.WebToolsEnabled())
	if len(cfg.LanguageServers) == 0 {
		registry.Unregister("builtin", "CodeNavigate")
	}
	// Preserve explicit routes supplied by the parent or caller.
	if _, suppliedRoute := opts.ProviderRegistry.ResolveRoute(cfg.Model); !suppliedRoute {
		models.Register(client)
		models.RouteDefault(cfg.Model, client.Name(), primaryBinding.httpBinding(cfg.ResolveProvider(cfg.Model).APIKey))
	}

	// Fallback model (Claude's --fallback-model): built lazily, used only if
	// the primary model's stream fails.
	var fallbackBinding modelBinding
	if cfg.FallbackModel != "" {
		// An unresolvable fallback stays unavailable rather than failing
		// construction, matching the primary provider remaining usable.
		fallbackBinding, _ = providerBinding(*cfg, models, cfg.FallbackModel, 1)
	}
	return &extensionRegistries{
		bus:             bus,
		registry:        registry,
		models:          models,
		primaryBinding:  primaryBinding,
		fallbackBinding: fallbackBinding,
	}, nil
}

// startSessionServices launches the event worker and the sandbox/hooks/MCP
// stack. MCP startup and SessionStart run only after every configured server
// has completed its handshake, so a failed candidate never produces a visible
// hook side effect.
func (a *Agent) startSessionServices(rootCancel context.CancelFunc, isolated bool, inheritedCapabilities []protocol.CapabilityRequest, inheritedExecutionGuard *sandbox.ExecutionGuard) error {
	if a.services.cfg.CodeModeEnabled && codemode.Available {
		a.services.registry.RegisterIn("code", &tools.RunCodeTool{})
	}
	a.services.cfg.ExecutorID = a.services.resources.ExecutorID()
	a.services.baseCfg.ExecutorID = a.services.cfg.ExecutorID
	a.services.sandbox = a.services.cfg.Sandbox()
	a.services.sandbox.SetExecutionGuard(inheritedExecutionGuard)
	if a.services.resources != nil {
		if err := a.services.resources.CheckOpen(); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
		a.services.resources.BindSandbox(a.services.sandbox)
	}

	for _, capability := range inheritedCapabilities {
		if err := applyCapability(a.services.sandbox, capability); err != nil {
			return fmt.Errorf("inherit parent sandbox capability: %w", err)
		}
	}
	a.services.hooks = hooks.NewManager(a.services.cfg.Hooks, hooks.Options{SessionID: a.conversation.sessionID, Workspace: a.services.cfg.Workspace, Mode: a.services.cfg.PermissionMode, Sandbox: a.services.sandbox, FailClosed: true})
	a.services.mcp.SetSandbox(a.services.sandbox)
	a.services.hooks.SetTranscript(a.services.cfg.SessionDir)
	if err := a.services.sandbox.CheckExecution(); err != nil {
		a.capabilities.capabilityRevoking = true
		a.emitStatus("%v", err)
	}
	if !isolated && !a.capabilities.capabilityRevoking {
		if err := a.services.mcp.StartChecked(a.lifecycle.rootCtx, a.services.cfg.MCPServers); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
		if err := a.services.mcp.RegisterTools(a.services.registry); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
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
	if a.services.cfg.ComputerUseEnabled && a.children.childState == nil {
		a.services.registry.RegisterIn("builtin", &tools.ComputerTool{})
	}
	a.services.registry.RegisterIn("builtin", tools.NewReadOutputTool())
	a.services.registry.RegisterIn("builtin", &readProjectMemoryTool{})
	a.services.registry.RegisterIn("builtin", &updateProjectMemoryTool{})
	a.services.registry.RegisterIn("builtin", &toolSearchTool{ag: a})
	a.services.registry.RegisterIn("builtin", &askUserQuestionTool{ag: a})
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
		// Step input loading reports any error before a provider request. Do
		// the initial scan with the same policy, leaving the store empty on
		// failure rather than caching unauthorized descriptions or bodies.
		_ = a.services.skills.LoadChecked(a.services.sandbox.ResolveRead, filepath.Join(home, ".ccdp", "skills"),
			filepath.Join(a.services.cfg.Workspace, ".ccdp", "skills"))
	}
}

// openTraceAndSupervisor opens the JSONL trace and attaches the session
// supervisor, then publishes the session-created lifecycle event.
func (a *Agent) openTraceAndSupervisor(opts Options) error {
	a.openTrace()
	a.services.hooks.SetTranscript(a.TracePath())
	a.streamEpoch = nextRuntimeID("stream")
	if opts.Supervisor != nil {
		a.services.supervisor = opts.Supervisor
	} else {
		a.services.supervisor = newSessionSupervisor(a, opts.memoryChildren)
		a.services.supervisorOwner = true
		if err := a.rootContext().Err(); err != nil {
			return err
		}
		if err := a.reconcileDescendantUsage(); err != nil {
			if a.rootContext().Err() != nil {
				return a.rootContext().Err()
			}
			a.emitStatus("token usage recovery incomplete: %v", err)
		}
	}
	a.emitSessionLifecycle(false)
	return a.rootContext().Err()
}

// emitSessionLifecycle publishes session events to observers.
func (a *Agent) emitSessionLifecycle(resumed bool) {
	if resumed {
		a.services.evbus.Emit(events.TopicSessionResumed, events.SessionEvent{ID: a.conversation.sessionID})
		return
	}
	a.services.evbus.Emit(events.TopicSessionCreated, events.SessionEvent{ID: a.conversation.sessionID, Workspace: a.services.cfg.Workspace})
}
