package agent

import (
	"context"
	"sync"
	"time"

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
	"ccdp/internal/session/replay"
	"ccdp/internal/sessionview"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
)

// These named state values make ownership explicit at call sites. Session
// transitions run in sessionOwner; construction owns unpublished initialization.
// Agent.mu guards read views and bounded producer inboxes during publication.
// The state values do not introduce their own mutexes or background workers.
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
	outputBudget int
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
	agentActivity    chan struct{}
	agentAttention   map[protocol.SessionID]string
	seenReceipts     map[protocol.CommandID]protocol.Receipt
	seenCommands     map[protocol.CommandID]string
	seenInputs       map[protocol.InputID]protocol.Receipt
	seenInputBody    map[protocol.InputID]string
	inputAttachments map[protocol.InputID]frozenInputAttachments
}

// watchState holds stream coalescing state. Guarded by mu; only the owner consumes publications.
type watchState struct {
	deferToolEvents       bool
	deferredToolEvents    []Event
	statePending          bool
	pendingViews          []protocol.Update
	streamTimerGeneration uint64
	publicationsLost      bool
	streamTimer           *time.Timer
	streamQueue           sessionview.StreamQueue
}

// sessionServices holds the long-lived dependencies owned by the runtime.
type sessionServices struct {
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
	host            *plugin.Host
	evbus           *events.Bus
	models          *plugin.ModelRegistry
	gohooks         *plugin.GoHooks
	sessionReg      *plugin.SessionRegistry
	mcp             *mcp.Manager
	skills          *skills.Store
	coding          *codingState
	resources       *tools.Resources
	// persistence is the sole business-fact writer for this session.
	persistence    *sessionPersistence
	persistenceErr error
}

// childRuntimeBindings holds the child-runtime hard-policy boundary.
type childRuntimeBindings struct {
	childState *childRuntimeState
	childSlots *childSlots
	childStep  *childStepSnapshot
}

// conversationState groups identity and history (guarded by mu where documented).
type conversationState struct {
	durable         replay.State
	recoveryBase    int
	recoveryLength  int
	recoveryView    []messages.Message
	recoveryHistory []messages.Message // synthetic recovery view, discarded at the next history commit
	goalOwner       goalAccountTarget
	goalOwnerID     string
	turnGoalID      string
	stepGoalID      string
	goalTurnCalls   int
	goalEmptyTurns  int
	sessionID       string
	createdAt       time.Time
	// Session lineage is set by Fork and recorded in the session log.
	parentID      string
	branchPoint   int
	branchSummary string
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
	workStartedAt time.Time
	workLabel     string
	interruptFlag bool
	turnCancel    context.CancelFunc
	turnCtx       context.Context
}

// sessionLifecycle groups the process/lifecycle machinery for a session.
type sessionLifecycle struct {
	rootCtx    context.Context
	rootCancel context.CancelFunc

	runStartedFlag bool

	turnWG      sync.WaitGroup
	operationWG sync.WaitGroup
	// sandboxOperationWG tracks scheduled work that must finish before a
	// tightened policy is published. Settings-mutating owner operations are
	// excluded because their context may be the operation performing the
	// publication; admission freezes all later additions once revocation starts.
	sandboxOperationWG sync.WaitGroup
	compactFailure     *compactFailureState
	closeOnce          sync.Once
	closeDone          chan struct{}
	closeErr           error

	closing  bool
	closed   bool
	settling bool // terminal event published before a queued turn starts
	phase    protocol.RuntimePhase
	workflow protocol.WorkflowState
	lastTurn *protocol.TurnOutcome
}

type capabilityState struct {
	// Session grants exist only in memory. Recovery initializes this map empty,
	// so prior approval events cannot restore a capability.
	capabilityGrants   map[string]protocol.CapabilityRequest
	capabilityRevision uint64
	capabilityRevoking bool
	settingsPreparing  bool
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
	cancellationTurn *uint64
	command          protocol.Command
	result           chan protocol.Receipt
}

type approvalAnswer struct {
	approve         bool
	remember        bool
	capabilityScope protocol.CapabilityScope
	// persisted is set by applyApproveTool, which commits the resolution before
	// waking the waiting tool. Direct package tests/legacy callers leave it
	// false, so requestApproval persists their answer itself.
	persisted bool
}

type frozenInputAttachments struct {
	Attachments []messages.ImageAttachment
	Frozen      bool
}
