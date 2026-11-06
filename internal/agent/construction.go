package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session/archive"
	"ccdp/internal/tools"
)

// New creates an agent with the given config and channels. The extension host
// is set up here: a domain event bus, scoped tools, model routing and hooks.
// Built-in tools and the default provider are registered directly.
func New(cfg *config.Config) (*Agent, error) {
	return newAgent(cfg, nil)
}

// NewWithOptions constructs the shared runtime used by child and guardian
// sessions. The effective configuration and provider/permission snapshots are
// copied at this boundary; no child may observe later parent mutations.
func NewWithOptions(cfg *config.Config, opts Options) (*Agent, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: config is required")
	}
	if err := validateAgentOptions(opts); err != nil {
		return nil, err
	}
	if !opts.EffectiveConfigFrozen {
		return nil, fmt.Errorf("agent: child effective config must be frozen")
	}
	return newAgentWithOptions(cfg, nil, opts)
}

// newAgent is the one construction path for both fresh sessions and an
// explicit resume. A normal New call must never attach an empty in-memory
// projection to an existing log; callers must use Resume (which supplies the
// restored projection) so execution always starts from durable history.
func newAgent(cfg *config.Config, initial *archive.Snapshot) (*Agent, error) {
	return newAgentWithOptions(cfg, initial, Options{})
}

func validateAgentOptions(opts Options) error {
	switch opts.Purpose {
	case "", childPurposeTask, childPurposeGuardian, childPurposeReview:
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

// newAgentWithOptions is the shared construction path for the main session,
// compact/fork helpers, and child/guardian sessions. It is the only place
// that creates an Agent, so provider, permissions, lifecycle hooks, and child
// hard-policy state cannot diverge across those runtimes.
func newAgentWithOptions(cfg *config.Config, initial *archive.Snapshot, opts Options) (*Agent, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: config is required")
	}
	if err := validateAgentOptions(opts); err != nil {
		return nil, err
	}
	baseCfg := cloneConfig(cfg)
	if opts.Purpose == "" {
		baseCfg.ExecutorID = ""
	}
	isolated := agentOptionsRequireIsolation(opts)
	effectiveCfg, err := buildEffectiveConfig(baseCfg, initial, isolated, opts)
	if err != nil {
		return nil, err
	}
	// A restored session's memory switch must survive later reload/cd
	// candidates, which are rebuilt from the launch baseline.
	baseCfg.EnableProjectMemory = effectiveCfg.EnableProjectMemory
	baseCfg.MemoryReadOnly, baseCfg.MemoryHistory = effectiveCfg.MemoryReadOnly, effectiveCfg.MemoryHistory
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

	// Domain registries and provider bindings.
	host, err := buildExtensionRegistries(cfg, opts, rootCancel)
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
		services: sessionServices{
			baseCfg:    baseCfg,
			trustStore: cfg.TrustStore,
			client:     host.primaryBinding.client,
			registry:   host.registry,
			gohooks:    plugin.NewGoHooks(),
			mcp:        mcp.NewManager(),
			perms:      cfg.PermManager(),
			evbus:      host.bus,
			models:     host.models,
			cfg:        cfg,
		},
		conversation: conversationState{
			sessionID:      sid,
			createdAt:      createdAt,
			branchPoint:    0,
			sessionStartAt: createdAt,
		},
		discovery: toolDiscoveryData{
			customPerms: customPerms,
			deferTools:  map[string]bool{},
			discovered:  map[string]bool{},
		},
		decisions:    approvalState{approvalCache: map[string]bool{}},
		capabilities: capabilityState{capabilityGrants: map[string]protocol.CapabilityRequest{}},
		modelsState: modelState{
			primaryBinding:  host.primaryBinding,
			fallbackBinding: host.fallbackBinding,
		},
		inbox: dedupState{
			seenReceipts:     make(map[protocol.CommandID]protocol.Receipt),
			seenCommands:     make(map[protocol.CommandID]string),
			seenInputs:       make(map[protocol.InputID]protocol.Receipt),
			seenInputBody:    make(map[protocol.InputID]string),
			inputAttachments: make(map[protocol.InputID]frozenInputAttachments),
		},
		publications: watchState{},
		sequences: seqState{
			settingsRev:    1,
			contextRev:     1,
			catalogVersion: 1,
			viewGeneration: 1,
		},
		owner: newSessionOwner(),
		lifecycle: sessionLifecycle{
			rootCtx:    rootCtx,
			rootCancel: rootCancel,
			closeDone:  make(chan struct{}),
			phase:      protocol.PhaseIdle,
		},
	}
	if cfg.NoSessionPersistence && opts.memoryCoding != nil {
		a.services.coding = &codingState{sessionID: sid, store: opts.memoryCoding.Clone()}
	}
	if err := a.finalizeConstruction(initial, opts, isolated, rootCancel); err != nil {
		return nil, err
	}
	a.constructionComplete = true
	a.startCatalogNotifications()
	a.startMemoryHistoryWorker()
	return a, nil
}

// abortConstruction is the single teardown path for a construction that must
// return an error. It unwinds resources in acquisition order; closeWriter is
// false only when the session writer never opened, so it must not be closed.
func (a *Agent) abortConstruction(rootCancel context.CancelFunc, closeWriter bool) {
	if closeWriter {
		_ = a.closePersistence()
	}
	rootCancel()
	if a.services.resources != nil {
		_ = a.services.resources.Close()
	}
	if a.services.mcp != nil {
		a.services.mcp.Close()
	}
}

// finalizeConstruction turns the freshly-built Agent literal into a fully
// initialized runtime: session lineage, durable snapshot replay, persistence,
// execution mode, session services, tool discovery, and the supervisor.
func (a *Agent) finalizeConstruction(initial *archive.Snapshot, opts Options, isolated bool, rootCancel context.CancelFunc) error {
	a.applySessionLineage(opts, isolated)
	a.applyInitialSnapshot(initial)
	if err := a.acquirePersistence(initial, opts, rootCancel); err != nil {
		return err
	}
	if err := a.replayOwnedStore(initial, opts, rootCancel); err != nil {
		return err
	}
	if err := a.startSessionServices(rootCancel, isolated, opts.InheritedCapabilities, opts.ExecutionGuard); err != nil {
		return err
	}
	a.finalizeToolsAndSkills(isolated)
	if err := a.openTraceAndSupervisor(opts); err != nil {
		a.abortConstruction(rootCancel, true)
		return err
	}
	if initial != nil && a.conversation.durable.Goal != nil {
		if err := a.recoverGoalAccounting(); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
		if err := a.restoreGoal(); err != nil {
			a.abortConstruction(rootCancel, true)
			return err
		}
	}
	return nil
}

// applySessionLineage records child/parent identity and the child hard-policy
// state derived from options.
func (a *Agent) applySessionLineage(opts Options, isolated bool) {
	a.conversation.goalOwner, a.conversation.goalOwnerID = opts.goalOwner, opts.goalOwnerID
	if opts.ParentSessionID != "" {
		// Child lineage is part of the new session's identity, not a live
		// pointer back into the parent. The option is copied at construction
		// and the normal snapshot/persistence path owns the durable projection.
		a.conversation.parentID = opts.ParentSessionID
	}
	if opts.Permissions != nil {
		a.services.perms = opts.Permissions.Clone()
	}
	a.children.childSlots = newChildSlots(a.services.cfg.MaxConcurrentAgents)
	a.children.guardianSlots = newChildSlots(1)
	if isolated {
		a.children.childState = &childRuntimeState{
			readOnlyWorkspace: opts.ReadOnlyWorkspace,
			purpose:           opts.Purpose,
			nonInteractive:    opts.NonInteractive,
			allowed:           cloneChildAllowed(opts.AllowedTools),
			parentSession:     opts.ParentSessionID,
			perms:             a.services.perms,
			access:            opts.access,
		}
	}
}

// applyInitialSnapshot restores history, pending inbox and usage from a resume
// snapshot so the runner can continue where the prior session left off.
func (a *Agent) applyInitialSnapshot(initial *archive.Snapshot) {
	if initial == nil {
		return
	}

	for id, attachments := range initial.PendingAttachments {
		a.inbox.inputAttachments[protocol.InputID(id)] = frozenInputAttachments{Attachments: messages.CloneImageAttachments(attachments), Frozen: true}
	}
	a.conversation.parentID, a.conversation.branchPoint, a.conversation.branchSummary = initial.ParentID, initial.BranchPoint, initial.BranchSummary
	a.sequences.turnSeq, a.sequences.stepSeq = initial.TurnSeq, initial.StepSeq
}

// acquirePersistence sets up the resources container, the active model
// binding, and the checkpoint/persistence stores, then opens the session
// writer for the fresh or resume path.
func (a *Agent) acquirePersistence(initial *archive.Snapshot, opts Options, rootCancel context.CancelFunc) error {
	resourceDir := filepath.Join(a.services.cfg.SessionDir, a.conversation.sessionID)
	if a.services.cfg.NoSessionPersistence {
		resourceDir = ""
	}
	a.services.resources = tools.NewResourcesWithContext(a.conversation.sessionID, resourceDir, a.lifecycle.rootCtx, a.services.cfg.ExecutorID, a.services.cfg.RuntimeRoot)
	a.modelsState.activeBinding = a.modelsState.primaryBinding
	openErr := error(nil)
	if initial == nil && a.services.cfg.SessionID != "" {
		openErr = a.openPersistenceMode(true, opts.journalBackend)
	} else {
		openErr = a.openPersistenceMode(false, opts.journalBackend)
	}
	if openErr != nil {
		a.abortConstruction(rootCancel, false)
		return openErr
	}
	return nil
}

// replayOwnedStore replays the now-owned durable log once more so a concurrent
// writer cannot leave a stale history/inbox projection at startup. It also
// restores input deduplication before new commands are admitted.
func (a *Agent) replayOwnedStore(initial *archive.Snapshot, opts Options, rootCancel context.CancelFunc) error {
	if initial == nil {
		return nil
	}
	if p := a.persistenceHandle(); p != nil {
		replayErr := error(nil)
		if len(opts.memoryResume) > 0 {
			replayErr = p.RestoreMemoryRun(opts.memoryResume, opts.memoryBlobs)
			a.transcript = p.TranscriptView()
		} else {
			replayErr = p.populateMemoryResume(initial)
		}
		if replayErr != nil {
			a.abortConstruction(rootCancel, true)
			return replayErr
		}
		owned, replayErr := archive.FromStore(p, a.conversation.sessionID)
		if replayErr != nil {
			a.abortConstruction(rootCancel, true)
			return replayErr
		}
		if owned != nil {
			if _, err := usageFromSession(owned.Usage); err != nil {
				a.abortConstruction(rootCancel, true)
				return err
			}
			if (owned.Model != "" && owned.Model != a.services.cfg.Model) || (owned.Workspace != "" && owned.Workspace != a.services.cfg.Workspace) {
				a.abortConstruction(rootCancel, true)
				return fmt.Errorf("agent: session settings changed while opening %q; retry resume", a.conversation.sessionID)
			}
			a.mu.Lock()
			a.conversation.createdAt = owned.CreatedAt
			a.conversation.recoveryHistory = nil
			recovered := a.conversation.durable.Recovered()
			if !reflect.DeepEqual(recovered.History, a.conversation.durable.History) {
				a.conversation.recoveryHistory = recovered.History
				a.conversation.recoveryBase = len(a.conversation.durable.History)
			}
			a.inbox.inputAttachments = make(map[protocol.InputID]frozenInputAttachments)
			for id, attachments := range owned.PendingAttachments {
				a.inbox.inputAttachments[protocol.InputID(id)] = frozenInputAttachments{Attachments: messages.CloneImageAttachments(attachments), Frozen: true}
			}
			a.conversation.parentID, a.conversation.branchPoint, a.conversation.branchSummary = owned.ParentID, owned.BranchPoint, owned.BranchSummary
			if owned.TurnSeq > a.sequences.turnSeq {
				a.sequences.turnSeq = owned.TurnSeq
			}
			if owned.StepSeq > a.sequences.stepSeq {
				a.sequences.stepSeq = owned.StepSeq
			}
			a.mu.Unlock()
		}
	}
	if err := a.recoverProcesses(); err != nil {
		a.abortConstruction(rootCancel, true)
		return err
	}
	if err := a.restoreInputDedup(); err != nil {
		a.abortConstruction(rootCancel, true)
		return err
	}
	return nil
}
