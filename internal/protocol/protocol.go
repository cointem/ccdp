// Package protocol contains the small, typed application protocol shared by
// the runtime and its clients. It deliberately has no I/O or dependency on
// the agent, TUI, storage, or a concrete provider.
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IDs are distinct aliases so values from different scopes cannot be mixed up
// accidentally at API boundaries. They remain strings on the wire.
type (
	SessionID   string
	CommandID   string
	OperationID string
	InputID     string
	TurnID      string
	StepID      string
	AttemptID   string
	CallID      string
)

func (id SessionID) String() string   { return string(id) }
func (id CommandID) String() string   { return string(id) }
func (id OperationID) String() string { return string(id) }
func (id InputID) String() string     { return string(id) }
func (id TurnID) String() string      { return string(id) }
func (id StepID) String() string      { return string(id) }
func (id AttemptID) String() string   { return string(id) }
func (id CallID) String() string      { return string(id) }

// Revision is the independently advancing version set exposed to clients.
// LogSeq is the durable ordering watermark. The other values scope prepared
// work and UI views.
type Revision struct {
	LogSeq         uint64 `json:"log_seq"`
	SettingsRev    uint64 `json:"settings_rev"`
	ContextRev     uint64 `json:"context_rev"`
	CatalogVersion uint64 `json:"catalog_version"`
	ViewGeneration uint64 `json:"view_generation"`
}

type Cursor struct {
	LogSeq         uint64 `json:"log_seq"`
	ViewGeneration uint64 `json:"view_generation"`
	StreamEpoch    string `json:"stream_epoch,omitempty"`
	EventSeq       uint64 `json:"event_seq,omitempty"`
}

// CommandType is the closed set of application commands. Command payloads
// are represented by exactly one typed pointer in Command; no untyped event
// bag or scalar fallback is accepted.
type CommandType string

const (
	CommandSubmitInput         CommandType = "submit_input"
	CommandSetGeneration       CommandType = "set_generation"
	CommandAnswerQuestion      CommandType = "answer_question"
	CommandSetModel            CommandType = "set_model"
	CommandSetExecutionMode    CommandType = "set_execution_mode"
	CommandSetPermissionPolicy CommandType = "set_permission_policy"
	CommandSetSandboxPolicy    CommandType = "set_sandbox_policy"
	CommandApproveTool         CommandType = "approve_tool"
	CommandApprovePlan         CommandType = "approve_plan"
	CommandInterrupt           CommandType = "interrupt"
	CommandStop                CommandType = "stop"
	CommandClearConversation   CommandType = "clear_conversation"
	CommandRemoveMessages      CommandType = "remove_messages"
	CommandRewindConversation  CommandType = "rewind_conversation"
	CommandCompact             CommandType = "compact"
	CommandFork                CommandType = "fork"
	CommandExternal            CommandType = "external_command"
	CommandApply               CommandType = "apply"
	CommandCheckpoint          CommandType = "checkpoint"
	CommandExport              CommandType = "export"
	CommandInit                CommandType = "init"
	CommandRunWorkflow         CommandType = "run_workflow"
	CommandQuery               CommandType = "query"
	CommandSetWorkspace        CommandType = "set_workspace"
	CommandReloadSettings      CommandType = "reload_settings"
	CommandTrustProject        CommandType = "trust_project"
	CommandClearMemory         CommandType = "clear_memory"
	CommandSaveSession         CommandType = "save_session"
)

type InputStrategy string

const (
	InputSteer    InputStrategy = "steer"
	InputFollowup InputStrategy = "followup"
)

type ExecutionMode string

const (
	ExecutionModeExecute ExecutionMode = "execute"
	ExecutionModePlan    ExecutionMode = "plan"
)

// PermissionPolicy is a credential-free view of permission settings. Mode is
// a string to keep protocol independent from the permissions implementation.
type PermissionPolicy struct {
	Mode        string   `json:"mode"`
	AlwaysAllow []string `json:"always_allow,omitempty"`
	AlwaysDeny  []string `json:"always_deny,omitempty"`
	Revision    uint64   `json:"revision"`
}

type SandboxPolicy struct {
	Mode                  string   `json:"mode"`
	AllowNetwork          bool     `json:"allow_network"`
	AdditionalDirectories []string `json:"additional_directories,omitempty"`
	DisallowedDirectories []string `json:"disallowed_directories,omitempty"`
	Revision              uint64   `json:"revision"`
}

type SubmitInput struct {
	ID       InputID       `json:"id,omitempty"`
	Text     string        `json:"text"`
	Strategy InputStrategy `json:"strategy,omitempty"`
	Images   []InputImage  `json:"images,omitempty"`
}

// MaxSubmitInputTextBytes bounds the user-controlled text carried by a
// submit_input command.  The limit is intentionally below the JSONL
// transaction limit: encoding/json may expand control characters and HTML
// punctuation, and admission also carries event metadata and the durable
// message projection in the same transaction.
const MaxSubmitInputTextBytes = 1 << 20

type SetModel struct {
	Model string `json:"model"`
}

type SetExecutionMode struct {
	Mode ExecutionMode `json:"mode"`
}

type SetPermissionPolicy struct {
	Policy PermissionPolicy `json:"policy"`
}

type SetSandboxPolicy struct {
	Policy SandboxPolicy `json:"policy"`
}

type ApproveTool struct {
	ApprovalID string `json:"approval_id"`
	Approve    bool   `json:"approve"`
	Remember   bool   `json:"remember,omitempty"`
}

type ApprovePlan struct {
	PlanID      string `json:"plan_id"`
	PlanVersion uint64 `json:"plan_version,omitempty"`
	Approve     bool   `json:"approve"`
}

type RemoveMessages struct {
	Count int `json:"count"`
}

type RewindConversation struct {
	Count int `json:"count"`
}

type Fork struct {
	Count int `json:"count"`
}

// Command is the canonical application command union. Type and the matching
// payload are required; a command carrying a different or multiple payloads
// is rejected before it enters the runtime.
type Command struct {
	Generation       *SetGeneration  `json:"generation,omitempty"`
	Answer           *AnswerQuestion `json:"answer,omitempty"`
	ID               CommandID       `json:"id,omitempty"`
	SessionID        SessionID       `json:"session_id"`
	Type             CommandType     `json:"type"`
	ExpectedRevision uint64          `json:"expected_revision,omitempty"`
	ExpectedRunID    RunID           `json:"expected_run_id,omitempty"`

	Input            *SubmitInput         `json:"input,omitempty"`
	Model            *SetModel            `json:"model,omitempty"`
	ExecutionMode    *SetExecutionMode    `json:"execution_mode,omitempty"`
	PermissionPolicy *SetPermissionPolicy `json:"permission_policy,omitempty"`
	SandboxPolicy    *SetSandboxPolicy    `json:"sandbox_policy,omitempty"`
	Approval         *ApproveTool         `json:"approval,omitempty"`
	Plan             *ApprovePlan         `json:"plan,omitempty"`
	Remove           *RemoveMessages      `json:"remove,omitempty"`
	Rewind           *RewindConversation  `json:"rewind,omitempty"`
	Fork             *Fork                `json:"fork,omitempty"`
	External         *ExternalCommand     `json:"external,omitempty"`
	Apply            *ApplyCommand        `json:"apply,omitempty"`
	Checkpoint       *CheckpointCommand   `json:"checkpoint,omitempty"`
	Export           *ExportCommand       `json:"export,omitempty"`
	Init             *InitCommand         `json:"init,omitempty"`
	Workflow         *WorkflowCommand     `json:"workflow,omitempty"`
	Query            *QueryCommand        `json:"query,omitempty"`
	Workspace        *WorkspaceCommand    `json:"workspace,omitempty"`
	Reload           *ReloadCommand       `json:"reload,omitempty"`
	TrustProject     *TrustProjectCommand `json:"trust_project,omitempty"`
	ClearMemory      *ClearMemoryCommand  `json:"clear_memory,omitempty"`
	SaveSession      *SaveSessionCommand  `json:"save_session,omitempty"`
}

// Normalize validates the discriminant/payload relationship and returns a
// canonical copy. It does not modify caller-owned payloads.
func (c Command) Normalize() (Command, error) {
	if c.ID == "" {
		return Command{}, errors.New("protocol: command id is required")
	}
	if c.SessionID == "" {
		return Command{}, errors.New("protocol: session_id is required")
	}
	if c.Type == "" {
		return Command{}, errors.New("protocol: command type is required")
	}

	payloads := 0
	if c.Generation != nil {
		payloads++
	}
	if c.Answer != nil {
		payloads++
	}
	if c.Input != nil {
		payloads++
	}
	if c.Model != nil {
		payloads++
	}
	if c.ExecutionMode != nil {
		payloads++
	}
	if c.PermissionPolicy != nil {
		payloads++
	}
	if c.SandboxPolicy != nil {
		payloads++
	}
	if c.Approval != nil {
		payloads++
	}
	if c.Plan != nil {
		payloads++
	}
	if c.Remove != nil {
		payloads++
	}
	if c.Rewind != nil {
		payloads++
	}
	if c.Fork != nil {
		payloads++
	}
	if c.External != nil {
		payloads++
	}
	if c.Apply != nil {
		payloads++
	}
	if c.Checkpoint != nil {
		payloads++
	}
	if c.Export != nil {
		payloads++
	}
	if c.Init != nil {
		payloads++
	}
	if c.Workflow != nil {
		payloads++
	}
	if c.Query != nil {
		payloads++
	}
	if c.Workspace != nil {
		payloads++
	}
	if c.Reload != nil {
		payloads++
	}
	if c.TrustProject != nil {
		payloads++
	}
	if c.ClearMemory != nil {
		payloads++
	}
	if c.SaveSession != nil {
		payloads++
	}

	requirePayload := func(ok bool) error {
		if payloads != 1 || !ok {
			return errors.New("protocol: command payload does not match type")
		}
		return nil
	}
	switch c.Type {
	case CommandSubmitInput:
		if err := requirePayload(c.Input != nil); err != nil {
			return Command{}, err
		}
		input := *c.Input
		c.Input = &input
		if strings.TrimSpace(c.Input.Text) == "" && len(c.Input.Images) == 0 {
			return Command{}, errors.New("protocol: input text is required")
		}
		if err := ValidateInputImages(c.Input.Images); err != nil {
			return Command{}, err
		}
		c.Input.Images = CloneInputImages(c.Input.Images)
		if len(c.Input.Text) > MaxSubmitInputTextBytes {
			return Command{}, fmt.Errorf("protocol: input text exceeds %d bytes", MaxSubmitInputTextBytes)
		}
		if c.Input.Strategy == "" {
			c.Input.Strategy = InputSteer
		}
		if c.Input.Strategy != InputSteer && c.Input.Strategy != InputFollowup {
			return Command{}, fmt.Errorf("protocol: unknown input strategy %q", c.Input.Strategy)
		}
	case CommandSetGeneration:
		if err := requirePayload(c.Generation != nil); err != nil {
			return Command{}, err
		}
		g := *c.Generation
		if g.ReasoningEffort == nil && g.Verbosity == nil {
			return Command{}, errors.New("protocol: a generation setting is required")
		}
		effort, verbosity := "", ""
		if g.ReasoningEffort != nil {
			effort = *g.ReasoningEffort
			g.ReasoningEffort = &effort
		}
		if g.Verbosity != nil {
			verbosity = *g.Verbosity
			g.Verbosity = &verbosity
		}
		if err := ValidateGeneration(effort, verbosity); err != nil {
			return Command{}, err
		}
		c.Generation = &g
	case CommandAnswerQuestion:
		if err := requirePayload(c.Answer != nil); err != nil {
			return Command{}, err
		}
		answer := CloneAnswer(*c.Answer)
		if answer.RequestID == "" {
			return Command{}, errors.New("protocol: question request_id is required")
		}
		if len(answer.Answers) > 4 {
			return Command{}, errors.New("protocol: too many answers")
		}
		c.Answer = &answer
	case CommandSetModel:
		if err := requirePayload(c.Model != nil); err != nil {
			return Command{}, err
		}
		model := *c.Model
		c.Model = &model
		if strings.TrimSpace(c.Model.Model) == "" {
			return Command{}, errors.New("protocol: model is required")
		}
	case CommandSetExecutionMode:
		if err := requirePayload(c.ExecutionMode != nil); err != nil {
			return Command{}, err
		}
		mode := *c.ExecutionMode
		c.ExecutionMode = &mode
		if c.ExecutionMode.Mode != ExecutionModeExecute && c.ExecutionMode.Mode != ExecutionModePlan {
			return Command{}, fmt.Errorf("protocol: unknown execution mode %q", c.ExecutionMode.Mode)
		}
	case CommandSetPermissionPolicy:
		if err := requirePayload(c.PermissionPolicy != nil); err != nil {
			return Command{}, err
		}
		policy := c.PermissionPolicy.Policy
		policy.AlwaysAllow = cloneStrings(policy.AlwaysAllow)
		policy.AlwaysDeny = cloneStrings(policy.AlwaysDeny)
		c.PermissionPolicy = &SetPermissionPolicy{Policy: policy}
	case CommandSetSandboxPolicy:
		if err := requirePayload(c.SandboxPolicy != nil); err != nil {
			return Command{}, err
		}
		policy := c.SandboxPolicy.Policy
		policy.AdditionalDirectories = cloneStrings(policy.AdditionalDirectories)
		policy.DisallowedDirectories = cloneStrings(policy.DisallowedDirectories)
		c.SandboxPolicy = &SetSandboxPolicy{Policy: policy}
	case CommandApproveTool:
		if err := requirePayload(c.Approval != nil); err != nil {
			return Command{}, err
		}
		approval := *c.Approval
		c.Approval = &approval
		if c.Approval.ApprovalID == "" {
			return Command{}, errors.New("protocol: approval_id is required")
		}
	case CommandApprovePlan:
		if err := requirePayload(c.Plan != nil); err != nil {
			return Command{}, err
		}
		plan := *c.Plan
		c.Plan = &plan
		if c.Plan.PlanID == "" {
			return Command{}, errors.New("protocol: plan_id is required")
		}
	case CommandRemoveMessages:
		if err := requirePayload(c.Remove != nil); err != nil {
			return Command{}, err
		}
		remove := *c.Remove
		c.Remove = &remove
	case CommandRewindConversation:
		if err := requirePayload(c.Rewind != nil); err != nil {
			return Command{}, err
		}
		rewind := *c.Rewind
		c.Rewind = &rewind
	case CommandFork:
		if err := requirePayload(c.Fork != nil); err != nil {
			return Command{}, err
		}
		fork := *c.Fork
		c.Fork = &fork
	case CommandExternal:
		if err := requirePayload(c.External != nil); err != nil {
			return Command{}, err
		}
		external, err := c.External.normalize()
		if err != nil {
			return Command{}, err
		}
		c.External = &external
	case CommandApply:
		if err := requirePayload(c.Apply != nil); err != nil {
			return Command{}, err
		}
		apply, err := c.Apply.normalize()
		if err != nil {
			return Command{}, err
		}
		c.Apply = &apply
	case CommandCheckpoint:
		if err := requirePayload(c.Checkpoint != nil); err != nil {
			return Command{}, err
		}
		checkpoint, err := c.Checkpoint.normalize()
		if err != nil {
			return Command{}, err
		}
		c.Checkpoint = &checkpoint
	case CommandExport:
		if err := requirePayload(c.Export != nil); err != nil {
			return Command{}, err
		}
		export, err := c.Export.normalize()
		if err != nil {
			return Command{}, err
		}
		c.Export = &export
	case CommandInit:
		if err := requirePayload(c.Init != nil); err != nil {
			return Command{}, err
		}
	case CommandRunWorkflow:
		if err := requirePayload(c.Workflow != nil); err != nil {
			return Command{}, err
		}
		workflow, err := c.Workflow.normalize()
		if err != nil {
			return Command{}, err
		}
		c.Workflow = &workflow
	case CommandQuery:
		if err := requirePayload(c.Query != nil); err != nil {
			return Command{}, err
		}
		query := *c.Query
		c.Query = &query
		switch query.Kind {
		case QueryConfig, QueryDoctor, QueryPermissions, QueryMemory, QueryMCP, QueryPlugins, QuerySkills, QueryStatus, QueryGitHub:
		default:
			return Command{}, fmt.Errorf("protocol: unknown query kind %q", query.Kind)
		}
	case CommandSetWorkspace:
		if err := requirePayload(c.Workspace != nil); err != nil {
			return Command{}, err
		}
		workspace := *c.Workspace
		workspace.Path = strings.TrimSpace(workspace.Path)
		if err := validateCommandText("workspace path", workspace.Path, maxCommandPathBytes, true); err != nil {
			return Command{}, err
		}
		c.Workspace = &workspace
	case CommandReloadSettings:
		if err := requirePayload(c.Reload != nil); err != nil {
			return Command{}, err
		}
	case CommandTrustProject:
		if err := requirePayload(c.TrustProject != nil); err != nil {
			return Command{}, err
		}
		trust := *c.TrustProject
		c.TrustProject = &trust
	case CommandClearMemory:
		if err := requirePayload(c.ClearMemory != nil); err != nil {
			return Command{}, err
		}
	case CommandSaveSession:
		if err := requirePayload(c.SaveSession != nil); err != nil {
			return Command{}, err
		}
	case CommandInterrupt, CommandStop, CommandClearConversation, CommandCompact:
		if payloads != 0 {
			return Command{}, errors.New("protocol: command does not accept a payload")
		}
	default:
		return Command{}, fmt.Errorf("protocol: unknown command type %q", c.Type)
	}
	return c, nil
}

func cloneStrings(src []string) []string {
	if src == nil {
		return nil
	}
	return append([]string{}, src...)
}

func (c Command) Validate() error {
	_, err := c.Normalize()
	return err
}

func NewSubmitInput(id CommandID, session SessionID, inputID InputID, text string, strategy InputStrategy) Command {
	return Command{ID: id, SessionID: session, Type: CommandSubmitInput,
		Input: &SubmitInput{ID: inputID, Text: text, Strategy: strategy}}
}

func NewSetModel(id CommandID, session SessionID, model string) Command {
	return Command{ID: id, SessionID: session, Type: CommandSetModel,
		Model: &SetModel{Model: model}}
}

func NewRunWorkflow(id CommandID, session SessionID, kind WorkflowKind, message string) Command {
	return Command{ID: id, SessionID: session, Type: CommandRunWorkflow,
		Workflow: &WorkflowCommand{Kind: kind, Message: message}}
}

// ReceiptStatus is the result of admission into the runtime command loop.
type ReceiptStatus string

const (
	ReceiptApplied   ReceiptStatus = "applied"
	ReceiptScheduled ReceiptStatus = "scheduled"
	ReceiptRejected  ReceiptStatus = "rejected"
)

type ErrorCode string

const (
	ErrorInvalidCommand ErrorCode = "invalid_command"
	ErrorWrongSession   ErrorCode = "wrong_session"
	ErrorStaleRevision  ErrorCode = "stale_revision"
	ErrorBusy           ErrorCode = "busy"
	ErrorClosed         ErrorCode = "closed"
	ErrorNotFound       ErrorCode = "not_found"
	ErrorInvalidState   ErrorCode = "invalid_state"
	ErrorInternal       ErrorCode = "internal"
)

type CommandError struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable,omitempty"`
	Field     string    `json:"field,omitempty"`
}

func (e *CommandError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

type Receipt struct {
	CommandID   CommandID     `json:"command_id"`
	SessionID   SessionID     `json:"session_id"`
	Status      ReceiptStatus `json:"status"`
	Revision    Revision      `json:"revision"`
	OperationID OperationID   `json:"operation_id,omitempty"`
	Error       *CommandError `json:"error,omitempty"`
}

func (r Receipt) Rejected() bool { return r.Status == ReceiptRejected }
func (r Receipt) Applied() bool  { return r.Status == ReceiptApplied }

type RuntimePhase string

const (
	PhaseIdle            RuntimePhase = "idle"
	PhasePreparing       RuntimePhase = "preparing"
	PhaseStreaming       RuntimePhase = "streaming"
	PhaseExecutingTools  RuntimePhase = "executing_tools"
	PhaseWaitingApproval RuntimePhase = "waiting_approval"
	PhaseCompacting      RuntimePhase = "compacting"
	PhaseStopping        RuntimePhase = "stopping"
	PhaseClosed          RuntimePhase = "closed"
)

type WorkflowState string

const (
	WorkflowOff              WorkflowState = "off"
	WorkflowDrafting         WorkflowState = "drafting"
	WorkflowAwaitingDecision WorkflowState = "awaiting_decision"
)

type ModelBinding struct {
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	Endpoint       string `json:"endpoint"`
	BindingVersion uint64 `json:"binding_version"`
}

type SettingsSnapshot struct {
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Verbosity       string           `json:"verbosity,omitempty"`
	Revision        uint64           `json:"revision"`
	Model           ModelBinding     `json:"model"`
	ExecutionMode   ExecutionMode    `json:"execution_mode"`
	Permission      PermissionPolicy `json:"permission"`
	Sandbox         SandboxPolicy    `json:"sandbox"`
	ContextWindow   int              `json:"context_window"`
	MaxReplyTokens  int              `json:"max_reply_tokens"`
	MaxTurns        int              `json:"max_turns"`
	MaxBudgetUSD    float64          `json:"max_budget_usd"`
}

type PendingSettings struct {
	Revision      uint64            `json:"revision"`
	Model         *ModelBinding     `json:"model,omitempty"`
	ExecutionMode *ExecutionMode    `json:"execution_mode,omitempty"`
	Permission    *PermissionPolicy `json:"permission,omitempty"`
	Sandbox       *SandboxPolicy    `json:"sandbox,omitempty"`
}

type ProviderSnapshot struct {
	ID           string   `json:"id"`
	Models       []string `json:"models,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Version      uint64   `json:"version"`
}

type ToolSnapshot struct {
	ID      string          `json:"id"`
	Version string          `json:"version,omitempty"`
	Effects []string        `json:"effects,omitempty"`
	Schema  json.RawMessage `json:"schema,omitempty"`
}

type CatalogSnapshot struct {
	Version   uint64             `json:"version"`
	Providers []ProviderSnapshot `json:"providers,omitempty"`
	Tools     []ToolSnapshot     `json:"tools,omitempty"`
}

type StepSnapshot struct {
	SessionID SessionID        `json:"session_id"`
	TurnID    TurnID           `json:"turn_id"`
	StepID    StepID           `json:"step_id"`
	Revision  Revision         `json:"revision"`
	Settings  SettingsSnapshot `json:"settings"`
	Catalog   CatalogSnapshot  `json:"catalog"`
}

type UsageSnapshot struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CachedTokens int     `json:"cached_tokens,omitempty"`
	Cost         float64 `json:"cost"`
	TurnCount    int     `json:"turn_count"`
}

// TurnOutcome is the recoverable terminal state of the latest turn. Watch
// streams are intentionally bounded and may be resynchronized, so clients
// must be able to distinguish a failed or cancelled turn from a successful
// one using Snapshot alone.
type TurnOutcomeStatus string

const (
	TurnRunning   TurnOutcomeStatus = "running"
	TurnSucceeded TurnOutcomeStatus = "succeeded"
	TurnFailed    TurnOutcomeStatus = "failed"
	TurnCancelled TurnOutcomeStatus = "cancelled"
)

type TurnOutcome struct {
	TurnID TurnID            `json:"turn_id"`
	Status TurnOutcomeStatus `json:"status"`
	Error  string            `json:"error,omitempty"`
}

type MessageView struct {
	ID          string   `json:"id,omitempty"`
	Role        string   `json:"role"`
	Content     string   `json:"content,omitempty"`
	ToolCallIDs []CallID `json:"tool_call_ids,omitempty"`
}

type InputView struct {
	ID        InputID       `json:"id"`
	Text      string        `json:"text"`
	Strategy  InputStrategy `json:"strategy"`
	State     string        `json:"state"`
	CreatedAt time.Time     `json:"created_at,omitempty"`
}

type ApprovalView struct {
	ID        string          `json:"id"`
	Tool      string          `json:"tool"`
	Reason    string          `json:"reason,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	CallID    CallID          `json:"call_id,omitempty"`
	PolicyRev uint64          `json:"policy_revision,omitempty"`
}

type PlanView struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	Text    string `json:"text"`
}

// EventKind identifies transient, displayable runtime activity carried by a
// Watch subscription. It is intentionally separate from UpdateType: a client
// can render progress while retaining the latest state Snapshot as the
// recovery source of truth.
type EventKind string

const (
	EventStatus          EventKind = "status"
	EventUserMessage     EventKind = "user_message"
	EventStream          EventKind = "stream"
	EventReasoning       EventKind = "reasoning"
	EventToolStarted     EventKind = "tool_started"
	EventToolProgress    EventKind = "tool_progress"
	EventToolResult      EventKind = "tool_result"
	EventApprovalRequest EventKind = "approval_request"
	EventQuestionRequest EventKind = "question_request"
	EventPlanReady       EventKind = "plan_ready"
	EventError           EventKind = "error"
	EventTurnDone        EventKind = "turn_done"
	EventUsage           EventKind = "usage"
	EventStateChanged    EventKind = "state_changed"
)

type ToolView struct {
	ID     CallID          `json:"id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Args   json.RawMessage `json:"args,omitempty"`
	Output string          `json:"output,omitempty"`
}

// EventView is the typed projection of a legacy agent event. It contains no
// provider credentials or mutable runtime pointers and is safe to retain
// after the update has been delivered.
type EventView struct {
	Question       *QuestionRequest `json:"question,omitempty"`
	Transcript     *TranscriptItem  `json:"transcript,omitempty"`
	Kind           EventKind        `json:"kind"`
	SessionID      SessionID        `json:"session_id,omitempty"`
	TurnID         TurnID           `json:"turn_id,omitempty"`
	StepID         StepID           `json:"step_id,omitempty"`
	AttemptID      AttemptID        `json:"attempt_id,omitempty"`
	CallID         CallID           `json:"call_id,omitempty"`
	MessageID      string           `json:"message_id,omitempty"`
	Text           string           `json:"text,omitempty"`
	Tool           *ToolView        `json:"tool,omitempty"`
	Approval       *ApprovalView    `json:"approval,omitempty"`
	Plan           *PlanView        `json:"plan,omitempty"`
	Phase          RuntimePhase     `json:"phase,omitempty"`
	Workflow       WorkflowState    `json:"workflow,omitempty"`
	ExecutionMode  ExecutionMode    `json:"execution_mode,omitempty"`
	PermissionMode string           `json:"permission_mode,omitempty"`
	Usage          *UsageSnapshot   `json:"usage,omitempty"`
	Error          string           `json:"error,omitempty"`
}

// SessionView is a read-only projection. Agent returns copies of all slices
// and nested values so a client cannot mutate or race runtime state.
type SessionView struct {
	Question          *QuestionRequest `json:"question,omitempty"`
	RunID             RunID            `json:"run_id,omitempty"`
	Transcript        []TranscriptItem `json:"transcript,omitempty"`
	TranscriptMore    bool             `json:"transcript_more,omitempty"`
	SessionID         SessionID        `json:"session_id"`
	Revision          Revision         `json:"revision"`
	Phase             RuntimePhase     `json:"phase"`
	Workflow          WorkflowState    `json:"workflow"`
	Busy              bool             `json:"busy"`
	Closing           bool             `json:"closing"`
	Settings          SettingsSnapshot `json:"settings"`
	Pending           *PendingSettings `json:"pending,omitempty"`
	Catalog           CatalogSnapshot  `json:"catalog"`
	Usage             UsageSnapshot    `json:"usage"`
	ContextUsedTokens int              `json:"context_used_tokens"`
	History           []MessageView    `json:"history,omitempty"`
	PendingInputs     []InputView      `json:"pending_inputs,omitempty"`
	Approval          *ApprovalView    `json:"approval,omitempty"`
	Plan              *PlanView        `json:"plan,omitempty"`
	LastTurn          *TurnOutcome     `json:"last_turn,omitempty"`
}

type UpdateType string

const (
	UpdateSnapshot       UpdateType = "snapshot"
	UpdateReceipt        UpdateType = "receipt"
	UpdateState          UpdateType = "state"
	UpdateStream         UpdateType = "stream"
	UpdateResyncRequired UpdateType = "resync_required"
)

type Update struct {
	Child       *ChildUpdate `json:"child,omitempty"`
	Type        UpdateType   `json:"type"`
	Cursor      Cursor       `json:"cursor"`
	Revision    Revision     `json:"revision"`
	Snapshot    *SessionView `json:"snapshot,omitempty"`
	Receipt     *Receipt     `json:"receipt,omitempty"`
	Event       *EventView   `json:"event,omitempty"`
	Text        string       `json:"text,omitempty"`
	OperationID OperationID  `json:"operation_id,omitempty"`
}

// Subscription is deliberately tiny: the runtime owns delivery and the
// consumer only observes a bounded stream. Close is idempotent.
type Subscription interface {
	Updates() <-chan Update
	Close() error
}

type SessionClient interface {
	Submit(context.Context, Command) (Receipt, error)
	Snapshot(context.Context) (SessionView, error)
	Watch(context.Context, Cursor) (Subscription, error)
}
