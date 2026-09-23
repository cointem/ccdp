package agent

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
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
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// Agent is the interactive coding agent. It owns the conversation history, the
// tool registry, the permission gate and the event channel that connects it to
// the TUI. It also hosts the plugin system: the built-in toolset
// and default LLM provider are loaded as plugins, and external plugins can
// register tools, providers, Go hooks and session callbacks at runtime.
//
// Concurrency model:
//   - Run() is the sole consumer of the command channel and dispatches work.
//   - Each turn runs in its own goroutine; at most one turn is active.
//   - Approval answers travel on approvalResp; interrupts are delivered via
//     turnCancel + an interrupt flag.
//
// Struct fields are grouped into a few embedded sub-structs purely for
// declaration readability (Go field promotion keeps call sites unchanged).
// All mutexes and the concurrency invariants stay on the Agent itself; the
// sub-structs hold no locks of their own.
//
// planState groups plan-mode bookkeeping (Claude Code's /plan). Guarded by mu.
type planState struct {
	planMode     bool
	pendingPlan  *PlanRequest
	planResp     chan bool
	planBaseMode permissions.Mode
}

// approvalState groups per-turn approval bookkeeping. Guarded by mu.
type approvalState struct {
	approvalCache map[string]bool
	// Pending approval (guarded by mu).
	pendingQuestion   *protocol.QuestionRequest
	questionResp      chan protocol.AnswerQuestion
	pendingApproval   *ApprovalRequest
	approvalResp      chan approvalAnswer
	approvalResolving bool
}

// modelState groups the primary + fallback model binding bookkeeping. Guarded
// by mu (see currentProvider / activeModelSnapshot).
type modelState struct {
	// primaryBinding and fallbackBinding each hold a complete provider identity
	// — client, endpoint, wire format and the config provider record that
	// produced them. Keeping them as whole bindings, rather than parallel
	// endpoint/kind fields, is what stops a model switch or fallback promotion
	// from pairing one provider's endpoint with another's wire format.
	primaryBinding  modelBinding
	fallbackBinding modelBinding
	usedFallback    bool
	activeModel     string
	// The active binding keeps model, endpoint/provider identity, and client as
	// one immutable unit, swapped atomically when a model change applies.
	activeBinding modelBinding
}

// budgetState groups per-turn tool-output accounting (guarded by mu).
type budgetState struct {
	outputBudget       int
	deferToolEvents    bool
	deferredToolEvents []Event
}

// turnMemoryState groups AutoMem / guardian / interrupt bookkeeping (guarded
// by mu).
type turnMemoryState struct {
	tokenBaseline struct {
		promptTokens int
		historyLen   int
	}
	guardianUses  int
	turnUserMsg   string
	turnTouched   []string
	turnSummary   string
	interruptNote bool
}

// seqState groups the monotonic revision/sequence counters (guarded by mu).
type seqState struct {
	settingsRev    uint64
	contextRev     uint64
	catalogVersion uint64
	turnSeq        uint64
	stepSeq        uint64
	viewGeneration uint64
	logSeq         uint64
}

// dedupState groups the protocol command/input dedup indexes (guarded by mu).
type dedupState struct {
	seenReceipts     map[protocol.CommandID]protocol.Receipt
	seenCommands     map[protocol.CommandID]string
	seenInputs       map[protocol.InputID]protocol.Receipt
	seenInputBody    map[protocol.InputID]string
	pendingInputs    []protocol.InputView
	inputAttachments map[protocol.InputID]frozenInputAttachments
}

// watchState groups the Watch subscription registry. Guarded by watchMu.
type watchState struct {
	watchers  map[uint64]*runtimeWatcher
	nextWatch uint64
}

// deps groups the injected service dependencies, mirroring codex's separate
// SessionServices struct. These are the long-lived references the agent owns.
type deps struct {
	supervisor      *SessionSupervisor
	supervisorOwner bool
	cfg             *config.Config
	baseCfg         config.Config // user/CLI config before workspace overrides
	trustStore      *config.TrustStore
	client          llm.Provider
	registry        *tools.Registry
	perms           *permissions.Manager
	hooks           *hooks.Manager
	sandbox         *sandbox.Sandbox
	wsInfo          *workspace.Info // cached workspace model
	host            *plugin.Host
	evbus           *events.Bus
	models          *plugin.ModelRegistry
	gohooks         *plugin.GoHooks
	sessionReg      *plugin.SessionRegistry
	mcp             *mcp.Manager
	skills          *skills.Store
	checkpoints     *checkpoint.Store
	resources       *tools.Resources
	// persistence is the sole business-fact writer for this session.
	persistence    *sessionPersistence
	persistenceErr error
}

// childStateGroups holds the child-runtime hard-policy boundary.
type childStateGroups struct {
	childState *childRuntimeState
	childSlots *childSlots
	childStep  *childStepSnapshot
}

// sessionData groups identity and history (guarded by mu where documented).
type sessionData struct {
	sessionID string
	createdAt time.Time
	history   []messages.Message
	// Session lineage is set by Fork and persisted by Save.
	parentID      string
	branchPoint   int
	branchSummary string
	events        chan Event // agent → UI
	// sessionStartAt fixes the environment "Date" once per session so the
	// system prompt prefix stays byte-identical across turns (prompt cache).
	sessionStartAt time.Time
	// modelSwitchMsg is injected into the next request's system prompt after a
	// model switch (Codex's ModelSwitchInstructions), then cleared.
	modelSwitchMsg string
}

// toolDiscoveryData groups per-custom-tool and deferred-tool admission state.
type toolDiscoveryData struct {
	// customPerms holds per-custom-tool permission overrides from config
	// ("allow" / "deny" / "ask").
	customPerms map[string]string
	// deferTools are tools not injected inline (MCP + config custom tools);
	// the model discovers them via ToolSearch. discovered tracks which of
	// them have been surfaced this session.
	deferTools map[string]bool
	discovered map[string]bool
}

// turnLifecycleData groups the per-turn inbox/interrupt state. Guarded by mu.
type turnLifecycleData struct {
	busy          bool
	interruptFlag bool
	stop          bool
	turnCancel    context.CancelFunc
	turnCtx       context.Context
	pendingMsgs   []string
}

// runtimeData groups the process/lifecycle machinery for a session.
type runtimeData struct {
	rootCtx        context.Context
	rootCancel     context.CancelFunc
	runStarted     chan struct{}
	runStartedFlag bool
	runDone        chan struct{}
	runOnce        sync.Once
	turnWG         sync.WaitGroup
	operationWG    sync.WaitGroup
	compactCancel  context.CancelFunc
	compactFailure *compactFailureState
	closeOnce      sync.Once
	closeDone      chan struct{}
	closeErr       error
	commands       chan runtimeCommand
	eventQueue     chan Event
	eventDone      chan struct{}
	eventWG        sync.WaitGroup
	closing        bool
	closed         bool
	settling       bool // terminal event published before a queued turn starts
	phase          protocol.RuntimePhase
	workflow       protocol.WorkflowState
	lastTurn       *protocol.TurnOutcome
}

type Agent struct {
	configuredModel string // launch configuration model; independent of the active selection
	transcript      *transcriptState
	finalizeRun     func(error) error // called after workers/hooks finish, before closing the writer
	streamEpoch     string
	deps
	eventSeq uint64 // guarded by watchMu

	// settingsCommitMu is the publication boundary for a resolved config and
	// its executable extension catalog. Typed queries take a read lock so they
	// cannot observe cfg from one generation with tools/MCP/policy from another.
	settingsCommitMu sync.RWMutex

	persistMu sync.Mutex

	childStateGroups

	sessionData

	mu sync.Mutex
	turnLifecycleData

	// approvalState groups per-turn approval bookkeeping. Guarded by mu.
	approvalState

	// Plan mode (Claude Code's /plan): while on, the model only proposes a plan;
	// mutating tools are blocked until the user approves it.
	planState

	// customPerms / deferTools / discovered
	toolDiscoveryData

	// fallbackBinding is used when the primary model fails (Claude's
	// --fallback-model). usedFallback latches per turn so each turn gets one
	// fallback attempt. activeModel tracks the model actually in use so usage
	// is priced with the right rate card after a fallback switch. primaryBinding
	// remembers the session's original provider binding so every turn can
	// restore it: a transient primary failure must not disable the primary for
	// the rest of the session.
	//
	// client, primaryBinding and cfg.Model are written by the canonical model
	// command and the fallback switch (turn goroutine), so every access goes
	// through a.mu (see currentProvider / activeModelSnapshot).
	modelState

	// outputBudget is the running count of tool-result characters delivered to
	// the model this turn (per-turn aggregate limit).
	budgetState

	// tokenBaseline anchors compaction estimates on the provider's real prompt
	// token count (Codex's BodyAfterPrefix idea): lastPromptTokens was reported
	// for a request built from baselineLen history messages; tokens for history
	// grown since then are estimated locally. Reset when history is rewritten.
	turnMemoryState

	// toolsTokensEstimate caches the tool-schema token footprint from the last
	// request prep, so the context-usage breakdown can split used tokens into
	// messages / tools / system without re-deriving tool schemas at snapshot.
	toolsTokensEstimate int

	// trace is the JSONL session trace file (nil when tracing is off).
	trace   *os.File
	traceMu sync.Mutex

	// approvalMu serializes approval requests so parallel tool workers never
	// clobber the single pending-approval slot.
	approvalMu sync.Mutex

	// usage accumulates token/cost accounting (guarded by mu).
	usage Usage

	// toolUses counts tool calls that reached execution (guarded by mu), so a
	// parent can show a child's tool activity without replaying its transcript.
	toolUses int

	// Runtime lifecycle and command admission. Submit is the sole application
	// entry and commands are normalized before application. The root context
	// owns every turn and event worker so Close can cancel and join all work
	// before releasing providers, hooks, traces, and tools.
	runtimeData

	// seqState groups the monotonic revision/sequence counters (guarded by mu).
	seqState
	// seenCommands retains only the canonical body digest. Keeping a full
	// command here made every accepted SubmitInput (including its potentially
	// large text payload) live for the lifetime of the session; durable command
	// facts remain the source of truth across resume.
	dedupState

	watchMu sync.Mutex
	watchState
}

type modelBinding struct {
	model      string
	provider   string // resolved provider/adapter name
	routeKind  string // "http" | "plugin" | "explicit"
	endpoint   string // base URL for this binding
	wire       string // normalized wire format ("chat" | "responses"), bound once with the endpoint
	providerID string // config provider id that served this model ("" for plugin routes)
	client     llm.Provider
	version    uint64
}

// httpBinding is the generated-route fingerprint for this binding. The
// credential is deliberately not a binding field — callers read it from the
// same config record that produced the endpoint and wire format.
func (b modelBinding) httpBinding(apiKey string) plugin.HTTPBinding {
	return plugin.HTTPBinding{Endpoint: b.endpoint, APIKey: apiKey, Wire: b.wire}
}

type runtimeCommand struct {
	command protocol.Command
	result  chan protocol.Receipt
}

type runtimeWatcher struct {
	owner  *Agent
	key    uint64
	ch     chan protocol.Update
	closed chan struct{}
	resync bool
}

type approvalAnswer struct {
	approve  bool
	remember bool
	// persisted is set by applyApproveTool, which commits the resolution before
	// waking the waiting tool. Direct package tests/legacy callers leave it
	// false, so requestApproval persists their answer itself.
	persisted bool
}

type frozenInputAttachments struct {
	Attachments []messages.ImageAttachment
	Frozen      bool
}

// New creates an agent with the given config and channels. The extension host
// is set up here: a domain event bus, a scoped tool registry, and the built-in
// plugins (toolset + default provider) are loaded through the same path an
// external plugin would use.
func New(cfg *config.Config, evCh chan Event) (*Agent, error) {
	return newAgent(cfg, evCh, nil)
}

// NewWithOptions constructs the shared runtime used by child and guardian
// sessions. The effective configuration and provider/permission snapshots are
// copied at this boundary; no child may observe later parent mutations.
func NewWithOptions(cfg *config.Config, evCh chan Event, opts Options) (*Agent, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: config is required")
	}
	if err := validateAgentOptions(opts); err != nil {
		return nil, err
	}
	if !opts.EffectiveConfigFrozen {
		return nil, fmt.Errorf("agent: child effective config must be frozen")
	}
	return newAgentWithOptions(cfg, evCh, nil, opts)
}

// newAgent is the one construction path for both fresh sessions and an
// explicit resume. A normal New call must never attach an empty in-memory
// projection to an existing log; callers must use Resume (which supplies the
// restored snapshot) so a later Save cannot replace durable history with an
// empty projection.
func newAgent(cfg *config.Config, evCh chan Event, initial *SessionSnapshot) (*Agent, error) {
	return newAgentWithOptions(cfg, evCh, initial, Options{})
}

func validateAgentOptions(opts Options) error {
	switch opts.Purpose {
	case "", childPurposeTask, childPurposeGuardian:
		// Valid purposes are intentionally closed: silently treating an unknown
		// purpose as an ordinary task would weaken its execution policy.
	default:
		return fmt.Errorf("agent: unknown child purpose %q", opts.Purpose)
	}
	if agentOptionsRequireIsolation(opts) && !opts.EffectiveConfigFrozen {
		return fmt.Errorf("agent: isolated options require a frozen effective config")
	}
	return nil
}

func agentOptionsRequireIsolation(opts Options) bool {
	return opts.Purpose != "" || opts.AllowedTools != nil || opts.NonInteractive || opts.ParentSessionID != ""
}

// applySessionSettingsToConfig restores the credential-free settings snapshot
// before providers, permissions, and the sandbox are constructed.  The
// session log is authoritative for mutable session policy; credentials and
// opaque provider configuration deliberately remain sourced from the current
// process configuration. A provider record owns its endpoint and credential
// together, so neither is ever restored or re-paired from the log here.
func applySessionSettingsToConfig(cfg *config.Config, settings session.Settings) {
	if cfg == nil {
		return
	}
	if settings.Workspace != "" {
		cfg.Workspace = settings.Workspace
	}
	if settings.Model != "" {
		cfg.Model = settings.Model
	}
	// settings.Endpoint is a record of which endpoint served the previous
	// request — possibly a provider-scoped or plugin endpoint — not a runtime
	// setting. Applying it to cfg.BaseURL would authenticate the top-level
	// credential against another provider's server, so the resumed binding is
	// always re-derived from the model plus the current config.
	if settings.PermissionPolicy != "" {
		cfg.PermissionMode = settings.PermissionPolicy
	}
	if settings.ExecutionMode == string(permissions.ExecutionModePlan) {
		cfg.PermissionMode = string(permissions.ModePlan)
	}
	if settings.SandboxPolicy != "" {
		cfg.SandboxMode = settings.SandboxPolicy
	}
	if settings.AllowNetworkSet || settings.AllowNetwork {
		cfg.SandboxAllowNetwork = settings.AllowNetwork
	}
	if settings.AlwaysAllow != nil {
		cfg.AlwaysAllow = append([]string(nil), settings.AlwaysAllow...)
	}
	if settings.AlwaysDeny != nil {
		cfg.AlwaysDeny = append([]string(nil), settings.AlwaysDeny...)
	}
	if settings.AdditionalDirectories != nil {
		cfg.AdditionalDirectories = append([]string(nil), settings.AdditionalDirectories...)
	}
	if settings.DisallowedDirectories != nil {
		cfg.DisallowedDirectories = append([]string(nil), settings.DisallowedDirectories...)
	}
	if settings.ContextWindow > 0 {
		cfg.ContextWindow = settings.ContextWindow
	}
	if settings.CompactThreshold > 0 {
		cfg.CompactThreshold = settings.CompactThreshold
	}
	if settings.MaxResultSizeChars > 0 {
		cfg.MaxResultSizeChars = settings.MaxResultSizeChars
	}
	if settings.MaxTurns > 0 {
		cfg.MaxTurns = settings.MaxTurns
	}
	if settings.MaxBudgetUSD > 0 {
		cfg.MaxBudgetUSD = settings.MaxBudgetUSD
	}
	if settings.GenerationOptionsSet {
		cfg.ReasoningEffort = settings.ReasoningEffort
		cfg.Verbosity = settings.Verbosity
	}
	if settings.MaxToolOutputCharsPerTurn > 0 {
		cfg.MaxToolOutputCharsPerTurn = settings.MaxToolOutputCharsPerTurn
	}
}

// newAgentWithOptions is the shared construction path for the main session,
// compact/fork helpers, and child/guardian sessions. It is the only place
// that creates an Agent, so provider, permissions, lifecycle hooks, and child
// hard-policy state cannot diverge across those runtimes.
func newAgentWithOptions(cfg *config.Config, evCh chan Event, initial *SessionSnapshot, opts Options) (*Agent, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: config is required")
	}
	if err := validateAgentOptions(opts); err != nil {
		return nil, err
	}
	baseCfg := cloneConfig(cfg)
	isolated := agentOptionsRequireIsolation(opts)
	effectiveCfg, err := buildEffectiveConfig(baseCfg, initial, isolated, opts)
	if err != nil {
		return nil, err
	}
	cfg = &effectiveCfg
	if err := validateResumeSession(cfg, initial); err != nil {
		return nil, err
	}

	rootParent := opts.RootContext
	if rootParent == nil {
		rootParent = context.Background()
	}
	if err := rootParent.Err(); err != nil {
		return nil, err
	}
	rootCtx, rootCancel := context.WithCancel(rootParent)

	// Extension host: domain registries, plugins, and provider bindings.
	host, err := buildExtensionHost(cfg, opts, rootCancel)
	if err != nil {
		return nil, err
	}

	// Custom external tools (config "tools"): register under a "custom" scope
	// and remember their permission overrides.
	customPerms := map[string]string{}
	for _, spec := range cfg.Tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		host.registry.RegisterIn("custom", tools.NewCommandTool(name, spec.Description, spec.Command, spec.InputSchema))
		customPerms[name] = strings.ToLower(spec.Permission)
	}

	sid := newSessionID()
	if cfg.SessionID != "" {
		sid = cfg.SessionID
	}
	createdAt := time.Now().UTC()
	if initial != nil && !initial.CreatedAt.IsZero() {
		createdAt = initial.CreatedAt
	}

	a := &Agent{
		configuredModel: baseCfg.Model,
		deps: deps{
			baseCfg:    baseCfg,
			trustStore: cfg.TrustStore,
			client:     host.primaryBinding.client,
			registry:   host.registry,
			gohooks:    host.ectx.Hooks,
			sessionReg: host.ectx.Session,
			mcp:        mcp.NewManager(),
			perms:      cfg.PermManager(),
			host:       host.host,
			evbus:      host.bus,
			models:     host.models,
			cfg:        cfg,
		},
		sessionData: sessionData{
			sessionID:      sid,
			createdAt:      createdAt,
			events:         evCh,
			branchPoint:    0,
			sessionStartAt: createdAt,
		},
		toolDiscoveryData: toolDiscoveryData{
			customPerms: customPerms,
			deferTools:  map[string]bool{},
			discovered:  map[string]bool{},
		},
		approvalState: approvalState{approvalCache: map[string]bool{}},
		modelState: modelState{
			primaryBinding:  host.primaryBinding,
			fallbackBinding: host.fallbackBinding,
		},
		dedupState: dedupState{
			seenReceipts:     make(map[protocol.CommandID]protocol.Receipt),
			seenCommands:     make(map[protocol.CommandID]string),
			seenInputs:       make(map[protocol.InputID]protocol.Receipt),
			seenInputBody:    make(map[protocol.InputID]string),
			inputAttachments: make(map[protocol.InputID]frozenInputAttachments),
		},
		watchState: watchState{watchers: make(map[uint64]*runtimeWatcher)},
		seqState: seqState{
			settingsRev:    1,
			contextRev:     1,
			catalogVersion: 1,
			viewGeneration: 1,
		},
		runtimeData: runtimeData{
			rootCtx:    rootCtx,
			rootCancel: rootCancel,
			runStarted: make(chan struct{}),
			runDone:    make(chan struct{}),
			closeDone:  make(chan struct{}),
			commands:   make(chan runtimeCommand, 64),
			eventQueue: make(chan Event, 512),
			eventDone:  make(chan struct{}),
			phase:      protocol.PhaseIdle,
			workflow:   protocol.WorkflowOff,
		},
	}
	if err := a.finalizeConstruction(initial, opts, isolated, rootCancel); err != nil {
		return nil, err
	}
	return a, nil
}

// buildEffectiveConfig merges the base config with project settings and, for a
// resume, the durable session settings snapshot, then applies the isolation
// overrides for child/guardian sessions. It returns the validated config the
// runtime is built from.
func buildEffectiveConfig(base config.Config, initial *SessionSnapshot, isolated bool, opts Options) (config.Config, error) {
	trustStore := base.TrustStore
	if trustStore == nil {
		trustStore = config.DefaultTrustStore()
	}
	if !opts.EffectiveConfigFrozen {
		projectCfg, err := config.LoadProjectSettingsTrusted(base.Workspace, trustStore)
		if err != nil {
			return config.Config{}, err
		}
		config.ApplyProjectSettings(&base, &projectCfg)
	}
	base.TrustStore = trustStore
	if initial != nil && initial.Settings != nil {
		applySessionSettingsToConfig(&base, *initial.Settings)
	}
	if !isolated && !opts.EffectiveConfigFrozen {
		if err := base.ApplyProjectPermission(); err != nil {
			return config.Config{}, err
		}
	}
	if isolated {
		// Child and guardian sessions never inherit lifecycle side effects from
		// the parent. Their tool admission is enforced separately by
		// ChildToolGate, but startup hooks/MCP/memory must also be disabled so a
		// child cannot run arbitrary commands merely by being constructed.
		if opts.Purpose == childPurposeTask {
			// Preserve only the parent's prompt/tool decision hooks. The child
			// never receives SessionStart/End or SubagentStart/Stop hooks; those
			// lifecycle events belong to the owning runtime and are not replayed.
			base.Hooks = childDecisionHooks(base.Hooks)
		} else {
			base.Hooks = hooks.Config{}
		}
		base.MCPServers = map[string]mcp.ServerConfig{}
		base.EnableGuardian = config.BoolPtr(false)
		base.EnableMemory = config.BoolPtr(false)
		if opts.Purpose == childPurposeGuardian {
			// Guardian's read-only gate is name based. Do not let a configured
			// custom command replace a built-in name such as Read and turn that
			// allowlisted name into arbitrary shell execution.
			base.Tools = nil
		}
	}
	if err := base.Validate(); err != nil {
		return config.Config{}, err
	}
	return base, nil
}

// validateResumeSession checks the restored session identity and, for a fresh
// session, guards against clobbering an existing durable log.
func validateResumeSession(cfg *config.Config, initial *SessionSnapshot) error {
	if cfg.SessionID != "" {
		if err := validateSessionID(cfg.SessionID); err != nil {
			return err
		}
	}
	if initial != nil {
		if err := validateSessionID(initial.ID); err != nil {
			return err
		}
		if cfg.SessionID != initial.ID {
			return fmt.Errorf("agent: resume config session id %q does not match snapshot %q", cfg.SessionID, initial.ID)
		}
	}
	if initial == nil && !cfg.NoSessionPersistence && cfg.SessionID != "" {
		existingEvents := filepath.Join(cfg.SessionDir, cfg.SessionID, "events.v1.jsonl")
		if _, err := os.Stat(existingEvents); err == nil {
			return fmt.Errorf("agent: session %q already exists; use Resume", cfg.SessionID)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("agent: inspect session %q: %w", cfg.SessionID, err)
		}
		legacyPath := filepath.Join(cfg.SessionDir, cfg.SessionID+".json")
		if _, err := os.Stat(legacyPath); err == nil {
			return fmt.Errorf("agent: legacy session %q exists; use Resume", cfg.SessionID)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("agent: inspect legacy session %q: %w", cfg.SessionID, err)
		}
	}
	return nil
}

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

// abortConstruction is the single teardown path for a construction that must
// return an error. It unwinds resources in acquisition order; closeWriter is
// false only when the session writer never opened, so it must not be closed.
func (a *Agent) abortConstruction(rootCancel context.CancelFunc, closeWriter bool) {
	if closeWriter {
		_ = a.closePersistence()
	}
	rootCancel()
	if a.resources != nil {
		_ = a.resources.Close()
	}
	if a.mcp != nil {
		a.mcp.Close()
	}
	if a.host != nil {
		_ = a.host.Close()
	}
}

// finalizeConstruction turns the freshly-built Agent literal into a fully
// initialized runtime: session lineage, durable snapshot replay, persistence,
// execution mode, session services, tool discovery, and the supervisor.
func (a *Agent) finalizeConstruction(initial *SessionSnapshot, opts Options, isolated bool, rootCancel context.CancelFunc) error {
	a.applySessionLineage(opts, isolated)
	a.applyInitialSnapshot(initial)
	if err := a.acquirePersistence(initial, rootCancel); err != nil {
		return err
	}
	if err := a.replayOwnedStore(initial, opts, rootCancel); err != nil {
		return err
	}
	a.initExecutionMode(initial)
	if err := a.startSessionServices(rootCancel, isolated); err != nil {
		return err
	}
	a.finalizeToolsAndSkills(isolated)
	a.openTraceAndSupervisor(opts)
	return nil
}

// applySessionLineage records child/parent identity and the child hard-policy
// state derived from options.
func (a *Agent) applySessionLineage(opts Options, isolated bool) {
	if opts.ParentSessionID != "" {
		// Child lineage is part of the new session's identity, not a live
		// pointer back into the parent. The option is copied at construction
		// and the normal snapshot/persistence path owns the durable projection.
		a.parentID = opts.ParentSessionID
	}
	if opts.Permissions != nil {
		a.perms = opts.Permissions.Clone()
	}
	a.childSlots = newChildSlots(a.cfg.MaxParallelTools)
	if isolated {
		a.childState = &childRuntimeState{
			purpose:        opts.Purpose,
			nonInteractive: opts.NonInteractive,
			allowed:        cloneChildAllowed(opts.AllowedTools),
			parentSession:  opts.ParentSessionID,
			perms:          a.perms,
		}
	}
}

// applyInitialSnapshot restores history, pending inbox and usage from a resume
// snapshot so the runner can continue where the prior session left off.
func (a *Agent) applyInitialSnapshot(initial *SessionSnapshot) {
	if initial == nil {
		return
	}
	a.history = cloneMessages(initial.History)
	a.pendingInputs = pendingInputSnapshot(initial.ID, initial.Pending, initial.PendingInputs)
	a.pendingMsgs = pendingTexts(a.pendingInputs)
	for id, attachments := range initial.PendingAttachments {
		a.inputAttachments[protocol.InputID(id)] = frozenInputAttachments{Attachments: cloneImageAttachments(attachments), Frozen: true}
	}
	a.usage = initial.Usage
	a.parentID, a.branchPoint, a.branchSummary = initial.ParentID, initial.BranchPoint, initial.BranchSummary
	a.turnSeq, a.stepSeq = initial.turnSeq, initial.stepSeq
}

// acquirePersistence sets up the resources container, the active model
// binding, and the checkpoint/persistence stores, then opens the session
// writer for the fresh or resume path.
func (a *Agent) acquirePersistence(initial *SessionSnapshot, rootCancel context.CancelFunc) error {
	resourceDir := filepath.Join(a.cfg.SessionDir, a.sessionID)
	if a.cfg.NoSessionPersistence {
		resourceDir = ""
	}
	a.resources = tools.NewResourcesWithContext(a.sessionID, resourceDir, a.rootCtx)
	a.activeBinding = a.primaryBinding
	if a.cfg.NoSessionPersistence {
		a.checkpoints = checkpoint.NewMemoryStore(a.sessionID)
	} else {
		a.checkpoints = checkpoint.NewStore(a.cfg.SessionDir, a.sessionID)
	}
	openErr := error(nil)
	if initial == nil && a.cfg.SessionID != "" {
		openErr = a.openPersistenceFresh()
	} else {
		openErr = a.openPersistence()
	}
	if openErr != nil {
		a.abortConstruction(rootCancel, false)
		return openErr
	}
	return nil
}

// replayOwnedStore replays the now-owned durable log once more so a concurrent
// writer cannot be overwritten by a stale history/pending projection during the
// first Save. Restores input dedup so a resumed command is not re-executed.
func (a *Agent) replayOwnedStore(initial *SessionSnapshot, opts Options, rootCancel context.CancelFunc) error {
	if initial == nil {
		return nil
	}
	if p := a.persistenceHandle(); p != nil {
		replayErr := error(nil)
		if len(opts.memoryResume) > 0 {
			replayErr = p.restoreMemoryRun(opts.memoryResume, opts.memoryBlobs)
			a.transcript = p.transcript
		} else {
			replayErr = p.populateMemoryResume(initial)
		}
		if replayErr != nil {
			a.abortConstruction(rootCancel, true)
			return replayErr
		}
		owned, replayErr := p.snapshotFromOwnedStore(a.sessionID)
		if replayErr != nil {
			a.abortConstruction(rootCancel, true)
			return replayErr
		}
		if owned != nil {
			if (owned.Model != "" && owned.Model != a.cfg.Model) || (owned.Workspace != "" && owned.Workspace != a.cfg.Workspace) {
				a.abortConstruction(rootCancel, true)
				return fmt.Errorf("agent: session settings changed while opening %q; retry resume", a.sessionID)
			}
			a.mu.Lock()
			a.createdAt = owned.CreatedAt
			a.history = cloneMessages(owned.History)
			a.pendingInputs = pendingInputSnapshot(a.sessionID, owned.Pending, owned.PendingInputs)
			a.pendingMsgs = pendingTexts(a.pendingInputs)
			a.inputAttachments = make(map[protocol.InputID]frozenInputAttachments)
			for id, attachments := range owned.PendingAttachments {
				a.inputAttachments[protocol.InputID(id)] = frozenInputAttachments{Attachments: cloneImageAttachments(attachments), Frozen: true}
			}
			a.usage = owned.Usage
			a.parentID, a.branchPoint, a.branchSummary = owned.ParentID, owned.BranchPoint, owned.BranchSummary
			if owned.turnSeq > a.turnSeq {
				a.turnSeq = owned.turnSeq
			}
			if owned.stepSeq > a.stepSeq {
				a.stepSeq = owned.stepSeq
			}
			a.mu.Unlock()
		}
	}
	if err := a.restoreInputDedup(); err != nil {
		a.abortConstruction(rootCancel, true)
		return err
	}
	return nil
}

// initExecutionMode seeds plan mode and the workflow phase for the restored or
// fresh session.
func (a *Agent) initExecutionMode(initial *SessionSnapshot) {
	initialMode := a.perms.CurrentMode()
	a.planBaseMode = initialMode
	if initialMode == permissions.ModePlan {
		a.planBaseMode = permissions.ModeDefault
		a.planMode = true
		a.workflow = protocol.WorkflowDrafting
		a.perms.SetMode(a.planBaseMode)
		a.cfg.PermissionMode = string(a.planBaseMode)
	}
	if initial != nil && initial.Workflow != nil {
		switch protocol.WorkflowState(initial.Workflow.Phase) {
		case protocol.WorkflowOff, protocol.WorkflowDrafting, protocol.WorkflowAwaitingDecision:
			a.workflow = protocol.WorkflowState(initial.Workflow.Phase)
		}
	}
}

// startSessionServices launches the event worker and the sandbox/hooks/MCP
// stack. MCP startup and SessionStart run only after every configured server
// has completed its handshake, so a failed candidate never produces a visible
// hook side effect.
func (a *Agent) startSessionServices(rootCancel context.CancelFunc, isolated bool) error {
	if a.events != nil {
		a.eventWG.Add(1)
		go a.eventLoop()
	}
	a.sandbox = a.cfg.Sandbox()
	a.checkpoints.SetExecutionBoundary(a.rootCtx, a.sandbox)
	a.hooks = hooks.NewManager(a.cfg.Hooks, hooks.Options{SessionID: a.sessionID, Workspace: a.cfg.Workspace, Mode: a.cfg.PermissionMode, Sandbox: a.sandbox, FailClosed: true})
	a.mcp.SetSandbox(a.sandbox)
	a.hooks.SetTranscript(a.cfg.SessionDir)
	if !isolated {
		if err := a.mcp.StartChecked(a.rootCtx, a.cfg.MCPServers); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
		a.mcp.RegisterTools(a.registry)
		if hookOut := a.runHookWithJournal(a.rootCtx, hooks.EventSessionStart, func(hctx context.Context) hooks.Output {
			return a.hooks.SessionStart(hctx)
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
	a.registry.RegisterIn("builtin", &toolSearchTool{ag: a})
	a.registry.RegisterIn("builtin", &askUserQuestionTool{ag: a})
	a.registry.RegisterIn("builtin", &enterPlanModeTool{ag: a})
	a.registry.RegisterIn("builtin", &exitPlanModeTool{ag: a})
	for _, spec := range a.cfg.Tools {
		if name := strings.TrimSpace(spec.Name); name != "" {
			a.deferTools[name] = true
		}
	}
	for _, names := range a.mcp.ToolNames() {
		for _, n := range names {
			a.deferTools[n] = true
		}
	}
	a.skills = skills.NewStore()
	if !isolated {
		home, _ := os.UserHomeDir()
		a.skills.Load(filepath.Join(home, ".ccdp", "skills"),
			filepath.Join(a.cfg.Workspace, ".ccdp", "skills"))
	}
}

// openTraceAndSupervisor opens the JSONL trace and attaches the session
// supervisor, then publishes the session-created lifecycle event.
func (a *Agent) openTraceAndSupervisor(opts Options) {
	a.openTrace()
	a.hooks.SetTranscript(a.TracePath())
	a.streamEpoch = nextRuntimeID("stream")
	if opts.Supervisor != nil {
		a.supervisor = opts.Supervisor
	} else {
		a.supervisor = newSessionSupervisor(a)
		a.supervisorOwner = true
	}
	a.emitSessionLifecycle(false)
}

// CreateCheckpoint snapshots the workspace under a git-based checkpoint and
// returns its id. Non-git workspaces return an error.
func (a *Agent) CreateCheckpoint(summary string) (string, error) {
	if err := a.admitCheckpoint("git stash create -u ccdp checkpoint"); err != nil {
		return "", err
	}
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
	if err := a.admitCheckpoint("git checkout <checkpoint> -- ."); err != nil {
		return err
	}
	if err := a.checkpoints.Restore(a.cfg.Workspace, id); err != nil {
		return err
	}
	workspace.Invalidate(a.cfg.Workspace)
	a.emitStatus("restored checkpoint %s", id)
	return nil
}

func (a *Agent) admitCheckpoint(command string) error {
	a.mu.Lock()
	perms := a.perms
	plan := a.planMode
	a.mu.Unlock()
	if plan {
		return fmt.Errorf("checkpoint operation is unavailable in plan mode")
	}
	if perms == nil {
		return fmt.Errorf("checkpoint permission manager is unavailable")
	}
	if denied, reason := perms.HardDeny("Bash", map[string]any{"command": command}); denied {
		return fmt.Errorf("checkpoint denied: %s", reason)
	}
	decision, reason := perms.Check("Bash", map[string]any{"command": command})
	if decision == permissions.DecisionDeny || decision == permissions.DecisionAsk {
		return fmt.Errorf("checkpoint requires explicit permission: %s", reason)
	}
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
func Resume(cfg *config.Config, snap *SessionSnapshot, evCh chan Event) (*Agent, error) {
	if snap == nil {
		return nil, fmt.Errorf("agent: session snapshot is required")
	}
	if err := validateSessionID(snap.ID); err != nil {
		return nil, err
	}
	if snap.CreatedAt.IsZero() {
		copy := *snap
		copy.CreatedAt = time.Now().UTC()
		snap = &copy
	}
	if err := prepareResumePersistence(cfg, *snap); err != nil {
		return nil, err
	}
	// Construct every session-scoped component with the restored id from the
	// start. This avoids creating hooks, traces and checkpoint stores under a
	// throwaway generated id before swapping to the snapshot.
	resumeCfg := *cfg
	if snap.Settings != nil {
		applySessionSettingsToConfig(&resumeCfg, *snap.Settings)
	}
	resumeCfg.SessionID = snap.ID
	if snap.Workspace != "" {
		abs, err := filepath.Abs(snap.Workspace)
		if err != nil {
			return nil, fmt.Errorf("agent: restore workspace: %w", err)
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("agent: restore workspace is not a directory: %s", snap.Workspace)
		}
		resumeCfg.Workspace = abs
	}
	if snap.Model != "" {
		resumeCfg.Model = snap.Model
	}
	a, err := newAgent(&resumeCfg, evCh, snap)
	if err != nil {
		return nil, err
	}
	if len(snap.PendingInputs) > 0 || len(snap.Pending) > 0 {
		pendingCount := len(snap.PendingInputs)
		if pendingCount == 0 {
			pendingCount = len(snap.Pending)
		}
		a.emitStatus("%d queued message(s) restored — they will run after the next turn", pendingCount)
	}
	if skipped := a.skippedRecords(); len(skipped) > 0 {
		a.emitStatus("%d session record(s) use event kinds this version cannot read and were skipped", len(skipped))
	}
	// Refresh mutable hook state, then re-discover deferred tools recorded in
	// the restored trace.
	a.syncHookContext()
	a.restoreDiscoveredFromStore()
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

// PluginNames returns the loaded plugin names in load order.
func (a *Agent) PluginNames() []string { return a.host.Names() }

// HostPending returns registered plugin names waiting for their dependencies.
func (a *Agent) HostPending() []string { return a.host.Pending() }

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

// PermissionMode returns the current permission mode.
func (a *Agent) PermissionMode() permissions.Mode { return a.perms.CurrentMode() }

// syncHookContext keeps lifecycle and tool hook payloads aligned with mutable
// agent state after mode, workspace or session changes.
func (a *Agent) syncHookContext() {
	a.mu.Lock()
	sessionID, ws := a.sessionID, a.cfg.Workspace
	a.mu.Unlock()
	a.hooks.SetContext(sessionID, ws, string(a.perms.CurrentMode()))
}

// SandboxMode returns the active sandbox mode.
func (a *Agent) SandboxMode() sandbox.Mode {
	return a.currentSandbox().CurrentMode()
}

// Usage returns a snapshot of the session's token/cost accounting.
func (a *Agent) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// ToolUses returns how many tool calls reached execution this session.
func (a *Agent) ToolUses() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolUses
}

// HooksList returns the configured hooks grouped by event.
func (a *Agent) HooksList() map[string][]string { return a.hooks.List() }

// PlanMode reports whether plan mode is active.
func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planMode
}

// SessionID returns the session identifier.
func (a *Agent) SessionID() string { return a.sessionID }

// WorkspaceLabel returns the absolute workspace path.
func (a *Agent) WorkspaceLabel() string { return a.cfg.Workspace }

// SessionDir returns the directory where sessions are persisted.
func (a *Agent) SessionDir() string { return a.cfg.SessionDir }

// currentProvider returns the provider currently in use (primary or fallback).
// Safe for concurrent use with SetModel and the fallback switch.
func (a *Agent) currentProvider() llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeBinding.client != nil {
		return a.activeBinding.client
	}
	return a.client
}

// activeModelSnapshot returns the model name requests should carry: the
// fallback model after a fallback switch, otherwise the configured primary.
// Safe for concurrent use with SetModel and the fallback switch.
func (a *Agent) activeModelSnapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeBinding.model != "" {
		return a.activeBinding.model
	}
	if a.activeModel != "" {
		return a.activeModel
	}
	return a.cfg.Model
}

// currentSandbox returns the sandbox in use. The pointer is swapped by
// SetWorkspace (UI goroutine), so tools and turn goroutines must not read the
// field directly.
func (a *Agent) currentSandbox() *sandbox.Sandbox {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sandbox
}

// hookCtx returns the context for hook / summarization calls that may run
// outside a live turn: the turn context while one is running, otherwise
// Background. A nil turn context would panic context.WithTimeout, and an
// already-cancelled one would instantly "time out" every hook.
func (a *Agent) hookCtx() context.Context {
	a.mu.Lock()
	ctx := a.turnCtx
	a.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return context.Background()
	}
	return ctx
}

// emit submits an event to the bounded runtime event queue. Producers never
// block on a slow UI; the event loop owns delivery to the caller's channel and
// stops with the root context. Every event is also appended to the session
// trace while the session is live.
func (a *Agent) emit(ev Event) {
	if ev.Type == EventToolResult {
		a.mu.Lock()
		if a.deferToolEvents {
			a.deferredToolEvents = append(a.deferredToolEvents, ev)
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
	a.writeTrace(ev)
	// Watch subscribers receive a typed projection independently from the
	// legacy Event channel. The projection is lossy under backpressure and is
	// recoverable through Snapshot, so this call never blocks the turn.
	a.publishEvent(ev)
	a.enqueueEvent(ev)
}

// emitTerminalEvent delivers the terminal event to the legacy adapter after
// publishTerminal has already sent its typed projection. Keeping this path
// separate prevents a second eventView call from broadcasting TurnDone after
// a queued next turn has announced its user message.
func (a *Agent) emitTerminalEvent(ev Event) {
	a.writeTrace(ev)
	a.enqueueEvent(ev)
}

// enqueueEvent is the legacy-channel half of emit. It intentionally stays
// outside any lifecycle or watcher lock: a slow legacy consumer must not hold
// the state snapshot lock while the runtime is settling a turn.
func (a *Agent) enqueueEvent(ev Event) {
	if a == nil || a.events == nil || a.eventQueue == nil {
		return
	}
	select {
	case <-a.eventDone:
		return
	case a.eventQueue <- ev:
	default:
		// Status and streaming previews are lossy by design. For durable
		// lifecycle/approval events, retain a bounded best-effort signal rather
		// than blocking the command/turn loop indefinitely.
		if ev.Type == EventStatus || ev.Type == EventStream || ev.Type == EventToolStream || ev.Type == EventReasoning {
			return
		}
		select {
		case <-a.eventDone:
		case a.eventQueue <- ev:
		default:
		}
	}
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

// Run is the agent's main command loop. It is the sole consumer of commands.
// Run is idempotent: callers may start it explicitly, while the first Submit
// also starts it when needed.
func (a *Agent) Run() {
	a.runOnce.Do(func() {
		a.mu.Lock()
		a.runStartedFlag = true
		a.mu.Unlock()
		close(a.runStarted)
		if a.supervisorOwner && a.supervisor != nil {
			a.supervisor.startRecovery()
		}
		defer close(a.runDone)
		for {
			select {
			case <-a.rootCtx.Done():
				return
			case req := <-a.commands:
				receipt := a.applyCommand(req.command)
				select {
				case req.result <- receipt:
				default:
				}
			}
		}
	})
}

// runTurn processes one user message through the full agent loop:
// Infer → ToolDispatch → ApprovalGate → Compact.
func (a *Agent) runTurn(input turnInput) {
	defer a.turnWG.Done()
	// turnFinished may hand off to a queued follow-up and add another turn to
	// the same WaitGroup. Keep this turn counted until that hand-off has either
	// completed or been rejected, so Close cannot observe a transient zero and
	// release session resources under the next turn.
	defer a.turnFinished()
	text := input.Text
	userMessage := input.message()
	// Turn lifecycle facts are admitted before hooks, history publication, or
	// any provider/tool side effect. The first model step is reserved here; a
	// later beginStep uses the same monotonically increasing sequence.
	a.mu.Lock()
	turnSeq := a.turnSeq
	firstStep := a.stepSeq + 1
	a.mu.Unlock()
	turnID := fmt.Sprintf("turn-%d", turnSeq)
	turnStarted := false
	if err := a.persistTurnStarted(turnID, fmt.Sprintf("step-%d", firstStep), string(input.ID)); err != nil {
		a.failTurnPersistence(err)
		return
	}
	turnStarted = true
	defer func() {
		if !turnStarted {
			return
		}
		outcome := "success"
		finishErr := ""
		if err := a.persistenceFailure(); err != nil {
			outcome = "error"
			finishErr = err.Error()
		} else {
			// EventError is observed synchronously by publishEvent, so the
			// canonical turn outcome is available before this deferred durable
			// TurnFinished fact. Provider/input failures must not be recorded as
			// successful turns merely because they did not poison persistence.
			a.mu.Lock()
			failed := a.lastTurn != nil && a.lastTurn.TurnID == protocol.TurnID(fmt.Sprintf("%d", turnSeq)) && a.lastTurn.Status == protocol.TurnFailed
			if failed {
				outcome = "error"
				finishErr = a.lastTurn.Error
			}
			a.mu.Unlock()
			if !failed && a.interrupted() {
				outcome = "cancelled"
			}
		}
		if err := a.persistTurnFinished(turnID, outcome, finishErr); err != nil {
			a.failTurnPersistence(err)
		}
	}()

	a.beginTurnState(text)

	// UserPromptSubmit hook can veto the message before any work happens. Tie
	// it to the runtime root so Close can stop a hanging external hook; there
	// is no per-step cancellation context until the first model step begins.
	hookCtx := a.rootCtx
	if hookCtx == nil {
		hookCtx = context.Background()
	}
	if ho := a.runHookWithJournal(hookCtx, hooks.EventUserPromptSubmit, func(hctx context.Context) hooks.Output {
		return a.hooks.UserPromptSubmit(hctx, text)
	}); ho.Decision == hooks.DecisionBlock || ho.Decision == hooks.DecisionDeny {
		reason := ho.Reason
		if reason == "" {
			reason = "blocked by UserPromptSubmit hook"
		}
		a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
		a.emit(Event{Type: EventError, Text: reason})
		return
	} else if ho.Continue != nil && !*ho.Continue {
		// {"continue": false} from a hook stops the agent loop entirely
		// (Claude Code hook semantics).
		reason := ho.Reason
		if reason == "" {
			reason = "stopped by UserPromptSubmit hook"
		}
		a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
		a.emit(Event{Type: EventError, Text: reason})
		return
	} else if ho.AdditionalContext != "" {
		// Hook-provided context rides along as a system message.
		if err := a.appendHistory(messages.Message{
			Role: messages.RoleSystem, Content: ho.AdditionalContext, CreatedAt: time.Now(),
		}); err != nil {
			a.failTurnPersistence(err)
			return
		}
	}

	a.emit(Event{Type: EventUserMsg, Text: text, MessageID: userMessage.ID})
	if !input.historyAppended {
		if err := a.appendHistory(userMessage); err != nil {
			a.failTurnPersistence(err)
			return
		}
	}

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
			return
		}
		// Compaction gate (Codex: compact between turns).
		if a.needsCompact() {
			a.emitStatus("context near limit, compacting…")
			a.compact()
			if a.interrupted() {
				return
			}
		}

		// -- Infer --
		// The turn context lives for the whole turn: the LLM stream and every
		// tool invocation share it, so an interrupt aborts all of them.
		turnCtx, turnCancel := context.WithCancel(a.rootCtx)
		a.mu.Lock()
		a.turnCtx = turnCtx
		a.turnCancel = turnCancel
		a.mu.Unlock()
		cancelStepContext := func() {
			turnCancel()
			a.mu.Lock()
			if a.turnCtx == turnCtx {
				a.turnCtx = nil
				a.turnCancel = nil
			}
			a.mu.Unlock()
		}

		// --max-turns: stop calling the model after the cap; report the
		// accumulated state so far instead of silently hanging.
		cfg := a.configSnapshot()
		if cfg.MaxTurns > 0 && steps >= cfg.MaxTurns {
			cancelStepContext()
			a.emitStatus("max turns reached (%d) — stopping", cfg.MaxTurns)
			return
		}
		// --max-budget: hard spending cap; stop when the session cost exceeds it.
		if cfg.MaxBudgetUSD > 0 && a.Usage().Cost >= cfg.MaxBudgetUSD {
			cancelStepContext()
			a.emitStatus("max budget reached ($%.2f of $%.2f) — stopping", a.Usage().Cost, cfg.MaxBudgetUSD)
			return
		}
		steps++

		a.emitStatus("thinking…")
		step, stepErr := a.beginStepChecked()
		if stepErr != nil {
			cancelStepContext()
			var inputErr *stepInputError
			if errors.As(stepErr, &inputErr) {
				a.emit(Event{Type: EventError, Text: inputErr.Error()})
			} else {
				a.failTurnPersistence(stepErr)
			}
			return
		}
		stepReleased := false
		releaseStep := func() {
			if stepReleased {
				return
			}
			stepReleased = true
			releaseStepLease(step)
			cancelStepContext()
		}
		req, requestErr := a.buildRequestForStepChecked(step)
		if requestErr != nil {
			releaseStep()
			var inputErr *stepInputError
			if errors.As(requestErr, &inputErr) {
				a.emit(Event{Type: EventError, Text: inputErr.Error()})
			} else {
				a.failTurnPersistence(requestErr)
			}
			return
		}
		call, prepareErr := prepareProviderCall(step.binding, req)
		if prepareErr != nil {
			releaseStep()
			detail := safeAttemptError(prepareErr, "", step.binding.endpoint)
			if detail == "" {
				detail = "request preparation failed"
			}
			a.emit(Event{Type: EventError, Text: "request preparation failed: " + detail})
			return
		}

		var streamed strings.Builder
		var res llm.StreamResult
		var streamErr error
		a.setPhase(protocol.PhaseStreaming)
		streamDone := make(chan struct{})
		go func() {
			purpose := "main"
			if a.childState != nil && a.childState.purpose != "" {
				purpose = a.childState.purpose
			}
			metadata := requestJournalMetadata{
				Config: step.cfg, SettingsRevision: step.settingsRev,
				ContextRevision: step.contextRev, CatalogVersion: step.catalogVersion,
				Turn: step.turn, Step: step.step, HistoryLen: len(step.history),
			}
			r, err := a.streamPrepared(turnCtx, purpose, call, metadata, func(delta string) {
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
			if a.handleStreamError(streamErr, releaseStep) {
				continue // switched to the fallback model; retry this step
			}
			return
		}

		callText := res.Text
		if callText == "" {
			// A provider may return the aggregate only (without invoking the
			// preview sink). Keep the old sink-only compatibility for providers
			// that stream deltas but return an empty aggregate.
			callText = streamed.String()
		}
		var calls []messages.ToolCall
		var toolCallErr error
		calls, toolCallErr = decodeProviderToolCalls(res.ToolCalls)
		if toolCallErr != nil {
			// Record the assistant prose, but never dispatch a malformed call.
			// In particular, do not let malformed/null/array arguments become
			// the historical {} default that could trigger a side effect.
			if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), messages.Message{Role: messages.RoleAssistant, Content: callText}); err != nil {
				releaseStep()
				a.failTurnPersistence(err)
				return
			}
			releaseStep()
			a.emit(Event{Type: EventError, Text: "invalid tool call: " + toolCallErr.Error()})
			return
		}
		assistant := messages.AssistantWithTools(callText, calls)
		assistant.ReasoningContent = res.Reasoning
		if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), assistant); err != nil {
			releaseStep()
			a.failTurnPersistence(err)
			return
		}

		// No tool calls → the assistant answered; turn is done, unless plan
		// mode is on (then the text is a plan awaiting approval), or the reply
		// was cut off by the token limit (then we continue automatically).
		if len(calls) == 0 {

			if a.inPlanMode() {
				if !a.requestPlanApproval(callText) {
					releaseStep()
					a.emit(Event{Type: EventStatus, Text: "plan not approved — no changes made"})
					return
				}
				// Approved: exit plan mode and execute the plan step by step.
				if err := a.setPlanMode(false); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				if err := a.appendHistory(messages.Message{
					Role: messages.RoleUser, Content: "The plan above is approved. Execute it now, following the plan step by step. Verify each step.",
					CreatedAt: time.Now(),
				}); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				releaseStep()
				continue
			}
			// The reply hit a provider or configured output limit; ask the
			// model to keep going (bounded, so it cannot loop forever).
			if res.FinishReason == "length" && continueCount < 3 {
				continueCount++
				a.emitStatus("reply hit the token limit — continuing…")
				if err := a.appendHistory(messages.Message{
					Role:      messages.RoleUser,
					Content:   "Continue your reply from where you left off.",
					CreatedAt: time.Now(),
				}); err != nil {
					releaseStep()
					a.failTurnPersistence(err)
					return
				}
				releaseStep()
				continue
			}
			// AutoMem: remember the final assistant answer as the turn's outcome.
			a.mu.Lock()
			a.turnSummary = callText
			a.mu.Unlock()
			releaseStep()
			return
		}

		// -- ToolDispatch + ApprovalGate --
		// Parallel dispatch (Codex-style) when configured; results are always
		// appended in call order so the conversation stays deterministic.
		//
		// Pi's failToolCallsFromTruncatedMessage: a reply cut off by the token
		// limit may carry tool calls whose streamed arguments were "salvaged"
		// into plausible-looking but incomplete JSON. Never execute those —
		// hand back an error result so the model re-issues the call with the
		// complete arguments.
		results, deferredToolEvents := a.executeToolCallsForStep(calls, step, res)
		// Every tool_call id MUST get a result message or the next request
		// violates the protocol (providers answer 400). dispatchTools fills
		// synthetic results for calls skipped by an interrupt, so we append
		// all of them unconditionally — never break early here.
		for i, r := range results {
			if err := a.appendHistoryForStep(fmt.Sprintf("step-%d", step.step), messages.NewToolResult(calls[i], r.output, r.isErr)); err != nil {
				// Do not publish a result whose history could not be confirmed.
				releaseStep()
				a.failTurnPersistence(err)
				return
			}
		}
		for _, ev := range deferredToolEvents {
			a.emit(ev)
		}

		// Steer (pi's one-at-a-time steering): a user message queued while the
		// turn was busy is injected at this tool-result boundary so the model
		// can react to it inside the running turn. Messages that arrive after
		// the last tool round drain as follow-up turns in turnFinished.
		if next, ok, err := a.claimPendingInputAndAppend(fmt.Sprintf("turn-%d", a.currentTurnSeq()), protocol.InputSteer); err != nil {
			releaseStep()
			a.failTurnPersistence(err)
			return
		} else if ok && next.Strategy == protocol.InputSteer {
			a.emit(Event{Type: EventUserMsg, Text: next.Text, MessageID: next.message().ID})
			a.emitStatus("steered your message into the running turn")
		}
		releaseStep()
		// Loop back to Infer with the tool results appended.
	}
}

// settleTurnLocked records the terminal outcome while the turn still owns the
// lifecycle lock. This is deliberately done before publishState and TurnDone:
// a watcher must never observe Busy=false with a running LastTurn, and an old
// terminal event must not be interpreted as belonging to a newly admitted turn.
// The caller must hold a.mu.
func (a *Agent) settleTurnLocked(turnID protocol.TurnID, saveErr error) {
	if a.lastTurn == nil || a.lastTurn.TurnID != turnID {
		a.lastTurn = &protocol.TurnOutcome{TurnID: turnID, Status: protocol.TurnRunning}
	}
	outcome := a.lastTurn
	switch {
	case saveErr != nil:
		outcome.Status = protocol.TurnFailed
		outcome.Error = "session persistence failed: " + saveErr.Error()
	case a.persistenceErr != nil:
		outcome.Status = protocol.TurnFailed
		outcome.Error = "session persistence failed: " + a.persistenceErr.Error()
	case outcome.Status == protocol.TurnFailed:
		// Preserve the provider/tool error already observed for this turn.
	case a.interruptFlag || a.stop || a.closing || a.closed:
		outcome.Status = protocol.TurnCancelled
		outcome.Error = ""
	default:
		outcome.Status = protocol.TurnSucceeded
		outcome.Error = ""
	}
}

// beginTurnState resets all per-turn bookkeeping before a new user message is
// processed: the approval cache, the tool-output/guardian budgets, the fallback
// budget (restoring the primary client), and the AutoMem turn tracking.
func (a *Agent) beginTurnState(text string) {
	a.mu.Lock()
	// The approval cache is per-turn: decisions made for one user request
	// never leak into the next one.
	a.approvalCache = map[string]bool{}
	a.mu.Unlock()
	a.resetOutputBudget()
	a.resetGuardianBudget()
	// Fallback budget resets per turn: one fallback attempt per turn, not per
	// session (a transient primary failure should not disable the primary for
	// the rest of the session). The primary client is restored too, so the
	// reset is real and not just cosmetic.
	a.mu.Lock()
	a.usedFallback = false
	primary := a.primaryBinding
	if primary.client != nil {
		a.client = primary.client
		// Fallback changes activeBinding for only the current turn; cfg.Model
		// and primaryBinding remain the authoritative primary binding for the
		// next turn.
		a.activeModel = primary.model
		primary.version = a.activeBinding.version + 1
		a.activeBinding = primary
	}
	a.mu.Unlock()

	// AutoMem: reset the per-turn tracking for this turn.
	a.mu.Lock()
	a.turnUserMsg = text
	a.turnTouched = nil
	a.turnSummary = ""
	a.mu.Unlock()
}

// executeToolCallsForStep runs the batch of tool calls admitted this step and
// returns them in call order with any events deferred while they were in
// flight. A reply cut off by the token limit never executes its "salvaged" but
// incomplete calls; those are returned as errors so the model re-issues them.
func (a *Agent) executeToolCallsForStep(calls []messages.ToolCall, step stepRuntime, res llm.StreamResult) ([]toolRunResult, []Event) {
	a.mu.Lock()
	a.deferToolEvents = true
	a.deferredToolEvents = nil
	a.mu.Unlock()
	journal := toolJournalContextForStep(step)
	var results []toolRunResult
	if res.FinishReason == "length" && len(calls) > 0 {
		a.emitStatus("reply truncated by the token limit — voiding %d incomplete tool call(s)", len(calls))
		results = make([]toolRunResult, len(calls))
		for i, tc := range calls {
			out := "not executed: the assistant message hit the token limit before this tool call finished streaming, " +
				"so its arguments may be incomplete. Re-issue the tool call with the full arguments."
			results[i] = toolRunResult{output: out, isErr: true}
			if err := a.finishUnexecutedToolWithStatus(withToolCallIndex(journal, i), tc, out, true, "denied"); err != nil {
				results[i].output = "session persistence failed: " + err.Error()
			}
			results[i].status = "denied"
			a.emit(toolEvent(tc, "denied", "voided (message truncated)"))
		}
		if journal.enabled {
			_, _ = a.projectToolResults(journal, calls, results)
		}
	} else {
		results = a.dispatchToolsForContext(calls, journal)
	}
	a.mu.Lock()
	a.deferToolEvents = false
	deferredToolEvents := append([]Event(nil), a.deferredToolEvents...)
	a.deferredToolEvents = nil
	a.mu.Unlock()
	return results, deferredToolEvents
}

// handleStreamError classifies a provider-stream failure and resolves it. It
// returns true when the caller should retry the step against the fallback model
// (the primary client failed once and the fallback budget for this turn is
// unused); false ends the turn, having released the step lease and emitted the
// appropriate error/interruption signal.
func (a *Agent) handleStreamError(streamErr error, releaseStep func()) bool {
	if isRequestJournalFailure(streamErr) {
		releaseStep()
		a.emit(Event{Type: EventError, Text: "request journal error: " + streamErr.Error()})
		return false
	}
	if a.interrupted() {
		releaseStep()
		a.mu.Lock()
		a.interruptNote = true
		a.mu.Unlock()
		a.emitStatus("stream interrupted")
		return false
	}
	// Fallback model (Claude's --fallback-model): one retry on the backup client,
	// then give up. activeModel follows so usage is priced with the fallback rate
	// card, and the request builder sends the fallback MODEL NAME to the fallback
	// endpoint (req.Model comes from activeModelSnapshot).
	a.mu.Lock()
	fallback := a.fallbackBinding
	if !a.usedFallback && fallback.client != nil {
		a.usedFallback = true
		a.client = fallback.client
		a.activeModel = fallback.model
		fallback.version = a.activeBinding.version + 1
		a.activeBinding = fallback
		a.mu.Unlock()
		releaseStep()
		a.emitStatus("primary model failed (%v) — falling back to %s", streamErr, fallback.model)
		return true
	}
	a.mu.Unlock()
	releaseStep()
	a.emit(Event{Type: EventError, Text: "LLM error: " + streamErr.Error()})
	return false
}

// turnFinished runs when a turn completes: it persists the session, publishes
// one explicitly-bound terminal event, and then drains any queued user
// messages. The settling phase covers even the no-queue path so a Submit that
// arrives during terminal publication cannot start a newer turn before the
// older TurnDone has been observed.
func (a *Agent) turnFinished() {
	a.mu.Lock()
	a.turnCancel = nil
	completedTurnSeq := a.turnSeq
	turnID := protocol.TurnID(fmt.Sprintf("%d", completedTurnSeq))
	if a.childStep != nil && a.childStep.turn == completedTurnSeq {
		a.childStep = nil
	}
	a.mu.Unlock()
	// AutoMem: fold this turn's request / touched files / outcome into the
	// session memory log before any queued turn resets the tracking fields.
	memoryErr := a.recordMemory()
	saveErr := a.Save()
	if saveErr == nil {
		saveErr = memoryErr
	}
	if saveErr != nil {
		a.markPersistenceFailure(saveErr)
	}

	// Keep the owner in a terminal hand-off state while LastTurn, phase, and
	// the explicit terminal event are made consistent. New Submit calls see
	// settling and are durably queued instead of racing this event.
	a.mu.Lock()
	a.ensureTypedPendingLocked()
	a.settling = true
	a.busy = false
	if a.closing || a.closed {
		a.phase = protocol.PhaseStopping
	} else {
		a.phase = protocol.PhaseIdle
	}
	a.settleTurnLocked(turnID, saveErr)
	a.mu.Unlock()
	if saveErr != nil {
		a.emit(Event{Type: EventError, TurnID: string(turnID), Text: "session persistence failed: " + saveErr.Error()})
	}
	// If a queued input was present at the terminal boundary, publishTerminal
	// keeps busy=true as a reservation and returns true. If none was present,
	// it has already made the session idle and published the complete boundary;
	// the old turn must not write state again after a new command is admitted.
	if !a.publishTerminal(turnID) {
		a.emitTerminalEvent(Event{Type: EventTurnDone, TurnID: string(turnID)})
		return
	}
	a.emitTerminalEvent(Event{Type: EventTurnDone, TurnID: string(turnID)})

	a.launchQueuedTurn(completedTurnSeq)
}

// idleAfterTurn resets the owner to a non-busy terminal phase under the lock.
func (a *Agent) idleAfterTurn() {
	a.mu.Lock()
	a.busy = false
	a.settling = false
	if a.closing || a.closed {
		a.phase = protocol.PhaseStopping
	} else {
		a.phase = protocol.PhaseIdle
	}
	a.mu.Unlock()
}

// launchQueuedTurn claims the oldest queued input and starts it as the next
// turn under the owner lock. The caller must already hold the busy reservation
// (from publishTerminal or a compaction reservation) so a competing turn cannot
// start. When no input is available, or cancellation/persistence stalls it, the
// session is left idle and false is returned.
func (a *Agent) launchQueuedTurn(completedTurnSeq uint64) bool {
	nextTurnID := fmt.Sprintf("turn-%d", completedTurnSeq+1)
	next, ok, err := a.claimPendingInputAndAppend(nextTurnID, "")
	if err != nil {
		a.markPersistenceFailure(err)
		a.idleAfterTurn()
		a.emit(Event{Type: EventError, Text: "queued input delivery failed: " + err.Error()})
		a.publishState()
		return false
	}
	if !ok {
		a.idleAfterTurn()
		a.publishState()
		return false
	}
	a.mu.Lock()
	if a.closing || a.closed || a.stop || a.interruptFlag || a.persistenceErr != nil {
		// The durable delivery is retained in history, but cancellation wins
		// over starting another provider request. The input is not re-queued.
		a.busy = false
		a.settling = false
		if !a.closing && !a.closed {
			a.phase = protocol.PhaseIdle
		}
		a.mu.Unlock()
		a.publishState()
		return false
	}
	a.turnSeq++
	a.turnWG.Add(1)
	// The queued input becomes the next turn only under this owner lock. Reset
	// the old terminal flags before publishing the new preparing state; a later
	// interrupt/stop cannot be erased by runTurn because it no longer clears
	// them asynchronously.
	a.interruptFlag = false
	a.stop = false
	a.settling = false
	a.busy = true
	a.phase = protocol.PhasePreparing
	a.mu.Unlock()
	a.publishState()
	go a.runTurn(next)
	return true
}

// dispatchTools executes a batch of tool calls. With maxParallelTools > 1 the
// calls run concurrently (bounded worker pool); approvals stay serialized so
// the user only ever sees one modal. Results come back in call order.
func (a *Agent) dispatchTools(calls []messages.ToolCall) []toolRunResult {
	return a.dispatchToolsForContext(calls, toolJournalContext{})
}

func (a *Agent) dispatchToolsForContext(calls []messages.ToolCall, jc toolJournalContext) []toolRunResult {
	n := jc.maxParallel
	if n == 0 {
		a.mu.Lock()
		n = a.cfg.MaxParallelTools
		a.mu.Unlock()
	}
	if n < 1 {
		n = 1
	}
	results := make([]toolRunResult, len(calls))
	runOne := func(idx int) {
		if a.interrupted() {
			results[idx] = toolRunResult{output: "interrupted before execution", isErr: true}
			if err := a.finishUnexecutedToolWithStatus(withToolCallIndex(jc, idx), calls[idx], results[idx].output, true, "cancelled"); err != nil {
				results[idx].output = "session persistence failed: " + err.Error()
			}
			results[idx].status = "cancelled"
			return
		}
		var status string
		callContext := withToolCallStatus(withToolCallIndex(jc, idx), &status)
		out, err := a.executeToolWithJournal(calls[idx], callContext)
		if status == "" {
			if err {
				status = "error"
			} else {
				status = "success"
			}
		}
		results[idx] = toolRunResult{output: out, isErr: err, status: status}
	}

	// A single pool for the entire batch is unsafe: a write/unknown call could
	// race reads before it and reads after it.  Run maximal consecutive runs of
	// explicitly read-only calls in a bounded pool, and execute every barrier
	// alone between those runs.  This preserves both side-effect ordering and
	// the original call-index order used by projection/budgeting.
	for start := 0; start < len(calls); {
		if !a.toolCallIsReadOnly(jc, calls[start]) {
			runOne(start)
			start++
			continue
		}
		end := start
		for end < len(calls) && a.toolCallIsReadOnly(jc, calls[end]) {
			end++
		}
		workers := n
		if workers > end-start {
			workers = end - start
		}
		if workers <= 1 {
			for idx := start; idx < end; idx++ {
				runOne(idx)
			}
		} else {
			jobs := make(chan int)
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for idx := range jobs {
						runOne(idx)
					}
				}()
			}
			for idx := start; idx < end; idx++ {
				jobs <- idx
			}
			close(jobs)
			wg.Wait()
		}
		start = end
	}
	if jc.enabled {
		_, _ = a.projectToolResults(jc, calls, results)
	}
	return results
}

func withToolCallIndex(jc toolJournalContext, index int) toolJournalContext {
	jc.callIndex = index
	return jc
}

func withToolCallStatus(jc toolJournalContext, sink *string) toolJournalContext {
	jc.statusSink = sink
	return jc
}

type toolRunResult struct {
	output string
	isErr  bool
	status string // explicit success/error/denied/cancelled outcome
}

// recordUsage accumulates token usage and cost for the session, then publishes
// the snapshot on the domain bus (TopicUsageUpdated). cachedTok counts prompt
// tokens served from a provider cache (OpenAI prompt_tokens_details). Cost is
// priced with the ACTIVE model, which may be the fallback after a switch.
func (a *Agent) recordUsage(inputTok, outputTok, cachedTok int) {
	a.recordUsageOpts(inputTok, outputTok, cachedTok, true)
}

// recordUsageNoBaseline accumulates usage without re-anchoring the compaction
// token baseline. Sub-agent loops report usage for requests built from their
// OWN history, not the parent's — anchoring on the parent history length
// would corrupt needsCompact until the next main-loop request.
func (a *Agent) recordUsageNoBaseline(inputTok, outputTok, cachedTok int) {
	a.recordUsageOpts(inputTok, outputTok, cachedTok, false)
}

func (a *Agent) recordUsageOpts(inputTok, outputTok, cachedTok int, anchorBaseline bool) {
	a.mu.Lock()
	a.usage.InputTokens += inputTok
	a.usage.OutputTokens += outputTok
	a.usage.CachedTokens += cachedTok
	a.usage.TurnCount++
	model := a.activeModel
	if model == "" {
		model = a.cfg.Model
	}
	a.usage.Cost += a.cfg.CostFor(model, inputTok, outputTok)
	u := a.usage
	// Anchor the compaction estimate: this usage was reported for a request
	// built from the current history length (the assistant reply and tool
	// results are appended after this point).
	if anchorBaseline {
		a.tokenBaseline.promptTokens = inputTok
		a.tokenBaseline.historyLen = len(a.history)
	}
	a.mu.Unlock()
	ev := Event{Type: EventUsage, Usage: &u}
	a.emit(ev)
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
	return a.executeToolWithJournal(tc, toolJournalContext{})
}

// executeToolWithJournal is the canonical dispatch path. The compatibility
// wrapper above keeps direct package callers on the pre-typed event behavior.
func (a *Agent) executeToolWithJournal(tc messages.ToolCall, journal toolJournalContext) (output string, isErr bool) {
	var execution *toolJournalExecution
	if journal.enabled {
		execution = &toolJournalExecution{ctx: journal}
		defer func() {
			if execution.finished {
				return
			}
			// No tool crosses the durable start boundary on a gate/approval
			// rejection. Derive that state from the control path, never from
			// the tool's human-readable output (which may legitimately contain
			// words such as "blocked" after a real execution). A cancelled
			// turn is distinct from a denied call for recovery/UI purposes.
			if execution.status == "" && !execution.started {
				if a.interrupted() {
					execution.setStatus("cancelled")
				} else {
					execution.setStatus("denied")
				}
			}
			if err := execution.finish(a, tc, output, isErr); err != nil {
				execution.setStatus("error")
				output = "session persistence failed: " + err.Error()
				isErr = true
			}
			output = a.toolOutputPreview(output, 0)
		}()
	}
	// Resolve the frozen implementation and apply the non-overridable hard
	// admission gates (child/guardian policy, plan mode, hard deny, strict
	// sandbox, config custom-perm). A custom "allow" shortcut runs immediately.
	tool, out, runNow, denied := a.hardAdmitTool(tc, journal, execution)
	if denied {
		return out, true
	}
	if runNow {
		return a.runToolWithJournal(tc, tool, journal, execution)
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
			return a.runToolWithJournal(tc, tool, journal, execution)
		}
	}

	// PreToolUse hooks can allow, deny or escalate a call.
	if ho := a.runHookWithJournal(a.turnCtx, hooks.EventPreToolUse, func(hctx context.Context) hooks.Output {
		return a.hooks.PreToolUse(hctx, tc.Name, tc.Arguments)
	}); ho.Decision != hooks.DecisionNone {
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
			return a.runToolWithJournal(tc, tool, journal, execution)
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

	return a.runToolWithJournal(tc, tool, journal, execution)
}

// hardAdmitTool resolves the frozen tool implementation and applies the
// non-overridable admission gates: child/guardian hard policy, plan-mode
// mutation block, explicit hard denies, strict-sandbox network blocks, and
// config-level custom-tool permissions. These must run before any hook,
// permission override, or Tool.Run; a name a custom/plugin scope can shadow is
// never used to classify capability. runNow is true only for the custom "allow"
// shortcut, which bypasses the remaining gates.
func (a *Agent) hardAdmitTool(tc messages.ToolCall, journal toolJournalContext, execution *toolJournalExecution) (tool tools.Tool, output string, runNow, denied bool) {
	var ok bool
	if journal.enabled && journal.toolLease != nil {
		tool, ok = journal.toolLease.Get(tc.Name)
	} else {
		tool, ok = a.registry.Get(tc.Name)
	}
	if denied, reason := ChildToolGate(a, tc); denied {
		a.emit(toolEvent(tc, "denied", reason))
		return nil, fmt.Sprintf("permission denied: %s", reason), false, true
	}
	if !ok {
		a.emit(toolEvent(tc, "error", "unknown tool "+tc.Name))
		return nil, fmt.Sprintf("Error: unknown tool %q", tc.Name), false, true
	}
	if purpose, _, _, child := ChildRuntime(a); child && purpose == childPurposeGuardian && !readOnlyImplementation(tool) {
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
	if denied, reason := a.perms.HardDeny(tc.Name, tc.Arguments); denied {
		a.emit(toolEvent(tc, "denied", reason))
		return nil, fmt.Sprintf("permission denied: %s", reason), false, true
	}

	// Sandbox × approval linkage: in strict sandbox mode, network tools are
	// blocked unless network access was explicitly allowed (Codex's
	// sandbox_mode ↔ approval matrix).
	if sb := a.currentSandbox(); sb != nil && sb.CurrentMode() == sandbox.ModeStrict && !sb.AllowNetwork {
		switch tc.Name {
		case "WebFetch", "WebSearch":
			a.emit(toolEvent(tc, "denied", "network blocked by strict sandbox"))
			return nil, fmt.Sprintf("permission denied: %s needs network access, which strict sandbox mode blocks (set sandbox_allow_network: true to permit)", tc.Name), false, true
		}
	}

	// Custom tools may carry a config-level permission override that bypasses
	// the normal approval gate ("allow" runs it, "deny" refuses it).
	if perm, isCustom := a.customPerms[tc.Name]; isCustom {
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

// runTool executes an approved tool call, publishing the tool pipeline domain
// events and running PostToolUse + Go post hooks.
func (a *Agent) runTool(tc messages.ToolCall, tool tools.Tool) (string, bool) {
	return a.runToolWithJournal(tc, tool, toolJournalContext{}, nil)
}

func (a *Agent) runToolWithJournal(tc messages.ToolCall, tool tools.Tool, journal toolJournalContext, execution *toolJournalExecution) (string, bool) {
	a.mu.Lock()
	cfg := cloneConfig(a.cfg)
	turnCtx := a.turnCtx
	sb := a.sandbox
	resources := a.resources
	skills := a.skills
	a.mu.Unlock()
	if turnCtx == nil {
		turnCtx = a.rootCtx
	}
	// Guardian review (Codex's guardian): high-risk calls are reviewed by a
	// read-only sub-agent before executing. Enabled via enable_guardian.
	if cfg.GuardianEnabled() && guardianRisk(tc.Name) {
		if err := a.guardianCheck(tc); err != nil {
			a.emit(toolEvent(tc, "denied", err.Error()))
			return err.Error(), true
		}
	}
	if execution != nil {
		if err := execution.start(a, tc); err != nil {
			return "session persistence failed: " + err.Error(), true
		}
	}
	finishToolOutput := func(raw string, failed bool) (string, bool) {
		if execution != nil {
			if err := execution.finish(a, tc, raw, failed); err != nil {
				return "session persistence failed: " + err.Error(), true
			}
			return a.toolOutputPreview(raw, cfg.MaxResultSizeChars), failed
		}
		// Direct/legacy callers retain the historical output-file and
		// per-agent clipping behavior. Canonical turns use the typed raw blob
		// and apply aggregate clipping once, in call order, in dispatchTools.
		raw = a.maybePersistResult(tc, raw)
		return a.clipAggregate(raw), failed
	}
	a.mu.Lock()
	a.toolUses++
	a.mu.Unlock()
	a.evbus.Emit(events.TopicToolPreExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "running"})
	a.emit(Event{Type: EventToolStart, Tool: &ToolEvent{ID: tc.ID, Name: tc.Name, Args: tc.Arguments}})
	a.emit(toolEvent(tc, "running", ""))
	a.emitStatus("%s running…", tc.Name)
	start := time.Now()
	tctx := a.buildToolContext(tc, turnCtx, cfg, sb, resources, skills)
	out, err := tool.Run(tctx)
	if err == nil && tc.Name == "TodoWrite" {
		// TodoStore is a mutable session resource. The tool's own store write
		// completes first; only then publish the typed task replacement so a
		// replay can restore the list without consulting a sidecar file.
		if taskErr := a.persistTasksSnapshot(); taskErr != nil {
			err = taskErr
		}
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	status := errString(err)
	if status == "" {
		status = "ok"
	}
	a.logv("tool %s %s in %s", tc.Name, status, elapsed)

	// Invalidate the workspace model after anything that may have changed files.
	switch tc.Name {
	case "Write", "Edit", "Bash":
		workspace.Invalidate(cfg.Workspace)
	}

	// AutoMem: remember files the turn modified so the memory log can say what
	// changed. Only Write/Edit carry a reliable file_path argument.
	if cfg.MemoryEnabled() {
		switch tc.Name {
		case "Write", "Edit":
			if p, _ := tc.Arguments["file_path"].(string); p != "" {
				a.recordTouched(p)
			}
		}
	}

	if err != nil {
		raw := fmt.Sprintf("%s failed after %s: %v", tc.Name, elapsed, err)
		if strings.TrimSpace(out) != "" {
			raw += "\n" + out
		}
		out, _ = finishToolOutput(raw, true)
		a.emit(toolEvent(tc, "error", out))
		a.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "error", Error: err.Error()})
		// PostToolUseFailure hooks observe the failure (non-blocking).
		a.runHookWithJournal(turnCtx, hooks.EventPostToolUseFailure, func(hctx context.Context) hooks.Output {
			return a.hooks.PostToolUseFailure(hctx, tc.Name, tc.Arguments, err.Error())
		})
		return out, true
	}

	// PostToolUse shell hooks can append context to the result.
	if ho := a.runHookWithJournal(turnCtx, hooks.EventPostToolUse, func(hctx context.Context) hooks.Output {
		return a.hooks.PostToolUse(hctx, tc.Name, tc.Arguments, out)
	}); ho.HookSpecificOutput != "" {
		out += "\n" + ho.HookSpecificOutput
	}
	// In-process Go post hooks run alongside.
	if extra := a.gohooks.RunPostTool(tc.Name, tc.Arguments, out); extra != "" {
		out += extra
	}
	out, _ = finishToolOutput(out, false)

	a.emit(toolEvent(tc, "success", out))
	a.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	a.evbus.Emit(events.TopicToolResult, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	return out, false
}

// buildToolContext assembles the *tools.Context handed to an approved tool,
// wiring Bash timeout, sub-agent/skills callbacks, session identity, and the
// streaming output sink. The registry/MCP leases are already frozen at the step
// boundary, so this context cannot observe a mid-stream plugin update.
func (a *Agent) buildToolContext(tc messages.ToolCall, turnCtx context.Context, cfg config.Config, sb *sandbox.Sandbox, resources *tools.Resources, skills *skills.Store) *tools.Context {
	var tctx *tools.Context
	if resources != nil {
		tctx = resources.Context(turnCtx, cfg.Workspace, sb)
	} else {
		// This branch is only for old hand-built Agent values in package tests.
		// New sessions always own a Resources container.
		tctx = &tools.Context{Context: turnCtx, WorkingDir: cfg.Workspace, SessionDir: cfg.SessionDir, Sandbox: sb}
	}
	tctx.Args = tc.Arguments
	tctx.Timeout = cfg.BashTimeout()
	if a.childState == nil {
		tctx.Sessions = a.Sessions()
		tctx.AgentCommandID = protocol.CommandID(stableID("agent-tool", struct{ Session, Turn, Call string }{a.sessionID, fmt.Sprint(a.turnSeq), tc.ID}))
	}
	tctx.Subagent = func(description, system string) (string, error) {
		results, err := a.runManagedTasks([]tools.SubagentTask{{Description: description, SystemPrompt: system}}, tc.ID)
		if err != nil {
			return "", err
		}
		if results[0].Error != "" {
			return results[0].Output, errors.New(results[0].Error)
		}
		return results[0].Output, nil
	}
	tctx.Subagents = func(tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
		return a.runManagedTasks(tasks, tc.ID)
	}
	tctx.Skills = skills
	if resources != nil {
		tctx.Owner = resources.Owner()
	}
	tctx.Notify = func(line string) {
		a.emit(Event{Type: EventToolStream, Tool: &ToolEvent{
			ID: tc.ID, Name: tc.Name, Status: "stream", Output: line,
		}})
	}
	return tctx
}

// requestApproval blocks until the user answers the approval request. It is
// safe for concurrent tool workers: approvalMu serializes the whole
// request-answer cycle so only one modal is ever pending (Codex's single
// approval slot).
//
// Within a turn the decision is memoized by permission SessionKey (Codex's
// approval cache): parallel workers submitting identical calls — and the
// Go-hook / shell-hook / permission gates asking about the same call — replay
// the first answer instead of re-prompting. The cache lives only for the
// current turn; lasting grants go through the remember flag / permission rules.
func (a *Agent) requestApproval(tc messages.ToolCall, reason string) (bool, bool) {
	// Non-interactive child/guardian sessions have no consumer for an approval
	// event. Deny synchronously instead of publishing a request that would hang
	// the child forever. ChildToolGate normally catches this before reaching
	// here; this guard also covers hook and direct package-level callers.
	if a != nil && a.childState != nil && a.childState.nonInteractive {
		return false, false
	}
	a.approvalMu.Lock()
	defer a.approvalMu.Unlock()

	// Replay a decision made earlier this turn for the same invocation. The
	// lookup happens after approvalMu is acquired, so the deciding worker has
	// always stored its answer by the time a second worker gets here.
	key := permissions.SessionKey(tc.Name, tc.Arguments)
	a.mu.Lock()
	if dec, ok := a.approvalCache[key]; ok {
		a.mu.Unlock()
		if dec {
			a.emitStatus("auto-approved %s (same call was already approved this turn)", tc.Name)
		}
		return dec, false
	}
	a.mu.Unlock()

	req := &ApprovalRequest{
		ID:      tc.ID,
		Tool:    tc.Name,
		Command: prettyArgs(tc.Arguments),
		Reason:  reason,
	}
	typedRequest := toolApprovalRequest(a, tc, reason)
	req.journalID = typedRequest.ApprovalID
	if err := a.persistApprovalRequested(typedRequest); err != nil {
		return false, false
	}
	a.mu.Lock()
	a.pendingApproval = req
	resp := make(chan approvalAnswer, 1)
	a.approvalResp = resp
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.approvalResp == resp {
			a.approvalResp = nil
			a.pendingApproval = nil
			a.approvalResolving = false
		}
		a.mu.Unlock()
	}()

	a.emit(Event{Type: EventApproval, Approval: req})

	a.mu.Lock()
	approvalCtx := a.turnCtx
	if approvalCtx == nil {
		approvalCtx = a.rootCtx
	}
	a.mu.Unlock()
	select {
	case ans := <-resp:
		decision := "deny"
		if ans.approve {
			decision = "allow"
		}
		if !ans.persisted {
			a.mu.Lock()
			if a.approvalResp != resp || a.approvalResolving {
				a.mu.Unlock()
				// A concurrent command already owns the durable resolution.  It
				// will deliver its answer through this channel; this branch can
				// only be reached for a stale direct sender, so do not write a
				// second ApprovalResolved fact.
				return ans.approve, ans.remember
			}
			a.approvalResolving = true
			a.mu.Unlock()
			if err := a.persistApprovalResolved(session.ApprovalResolution{ApprovalID: typedRequest.ApprovalID, Decision: decision, Reason: reason, ResolvedBy: "user"}); err != nil {
				return false, false
			}
		}
		a.mu.Lock()
		a.approvalCache[key] = ans.approve
		a.mu.Unlock()
		return ans.approve, ans.remember
	case <-approvalCtx.Done():
		a.mu.Lock()
		if a.approvalResp != resp {
			a.mu.Unlock()
			return false, false
		}
		if a.approvalResolving {
			a.mu.Unlock()
			// The command path won the race but has not necessarily sent its
			// buffered answer yet. Wait for that answer rather than writing a
			// competing cancel fact under the same ApprovalID.
			ans := <-resp
			return ans.approve, ans.remember
		}
		a.approvalResolving = true
		a.mu.Unlock()
		if err := a.persistApprovalResolved(session.ApprovalResolution{ApprovalID: typedRequest.ApprovalID, Decision: "cancel", Reason: "interrupted", ResolvedBy: "runtime"}); err != nil {
			return false, false
		}
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

// setPlanMode is the internal mid-turn facade for the same execution-mode
// transition used by external commands. It intentionally does not bypass the
// unified plan/permission bookkeeping.
func (a *Agent) setPlanMode(on bool) error {
	return a.setExecutionMode(on)
}

// requestPlanApproval surfaces the proposed plan and blocks until the user
// approves or rejects it. Returns true to execute.
func (a *Agent) requestPlanApproval(plan string) bool {
	a.approvalMu.Lock()
	defer a.approvalMu.Unlock()
	if a != nil && a.childState != nil && a.childState.nonInteractive {
		return false
	}
	req := &PlanRequest{ID: fmt.Sprintf("plan-%d", time.Now().UnixNano()), Plan: plan}
	awaiting := session.WorkflowState{Phase: string(protocol.WorkflowAwaitingDecision), PlanID: req.ID, PlanVersion: planViewVersion}
	if err := a.persistWorkflowFact(awaiting); err != nil {
		return false
	}
	a.mu.Lock()
	resp := make(chan bool, 1)
	a.pendingPlan = req
	a.planResp = resp
	a.workflow = protocol.WorkflowAwaitingDecision
	a.mu.Unlock()
	a.publishState()
	defer func() {
		a.mu.Lock()
		if a.planResp == resp {
			a.planResp = nil
			a.pendingPlan = nil
		}
		a.mu.Unlock()
	}()

	a.emit(Event{Type: EventPlan, Plan: req})
	a.emitStatus("plan ready — approve (y) to execute, deny (n) to reject")

	a.mu.Lock()
	approvalCtx := a.turnCtx
	if approvalCtx == nil {
		approvalCtx = a.rootCtx
	}
	a.mu.Unlock()
	select {
	case ok := <-resp:
		if !ok {
			drafting := session.WorkflowState{Phase: string(protocol.WorkflowDrafting), PlanID: req.ID, PlanVersion: planViewVersion}
			if err := a.persistWorkflowFact(drafting); err != nil {
				return false
			}
			a.mu.Lock()
			if a.planMode {
				a.workflow = protocol.WorkflowDrafting
			}
			a.mu.Unlock()
		}
		a.publishState()
		return ok
	case <-approvalCtx.Done():
		drafting := session.WorkflowState{Phase: string(protocol.WorkflowDrafting), PlanID: req.ID, PlanVersion: planViewVersion}
		if err := a.persistWorkflowFact(drafting); err != nil {
			return false
		}
		a.mu.Lock()
		if a.planMode {
			a.workflow = protocol.WorkflowDrafting
		}
		a.mu.Unlock()
		a.publishState()
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
	compactCancel := a.compactCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if compactCancel != nil {
		compactCancel()
	}
}

// clearHistory wipes the conversation (keeps the system message).
func (a *Agent) clearHistory() {
	if a.isBusy() {
		a.emitStatus("cannot clear while a turn is running")
		return
	}
	if err := a.clearHistoryCommand(fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to clear history: %v", err)
	}
}

// removeLast drops the trailing n messages from the conversation.
func (a *Agent) removeLast(n int) {
	if a.isBusy() {
		a.emitStatus("cannot remove messages while a turn is running")
		return
	}
	if err := a.removeMessagesCommand(n, fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to remove messages: %v", err)
	}
}

// rewindTo keeps only the first n messages, dropping everything after them.
func (a *Agent) rewindTo(n int) {
	if a.isBusy() {
		a.emitStatus("cannot rewind while a turn is running")
		return
	}
	if err := a.rewindMessagesCommand(n, fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to rewind history: %v", err)
	}
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
	// Session IDs are also directory names and can be generated concurrently
	// by independent processes. A cryptographically random base32 value avoids
	// the microsecond timestamp collisions of the old format without adding a
	// process-local counter that would still collide across processes.
	return cryptorand.Text()
}
