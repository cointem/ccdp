package session

import (
	"bytes"
	"ccdp/internal/protocol"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

const sha256HexLength = 64

// EventType is the stable name written in the event log.  EventType is not an
// open string bag: only the concrete event types declared in this package can
// satisfy Event, and the decoder rejects names it does not know.
type EventType string

const (
	EventTypeSessionCreated       EventType = "SessionCreated"
	EventTypeSessionImported      EventType = "SessionImported"
	EventTypeSessionClosed        EventType = "SessionClosed"
	EventTypeInputQueued          EventType = "InputQueued"
	EventTypeInputDelivered       EventType = "InputDelivered"
	EventTypeInputCancelled       EventType = "InputCancelled"
	EventTypeCommandScheduled     EventType = "CommandScheduled"
	EventTypeCommandCompleted     EventType = "CommandCompleted"
	EventTypeSettingsChanged      EventType = "SettingsChanged"
	EventTypeWorkflowChanged      EventType = "WorkflowChanged"
	EventTypeTurnStarted          EventType = "TurnStarted"
	EventTypeTurnFinished         EventType = "TurnFinished"
	EventTypeRequestPrepared      EventType = "RequestPrepared"
	EventTypeAttemptFinished      EventType = "AttemptFinished"
	EventTypeAssistantCommitted   EventType = "AssistantCommitted"
	EventTypeToolStarted          EventType = "ToolStarted"
	EventTypeToolFinished         EventType = "ToolFinished"
	EventTypeToolResultsProjected EventType = "ToolResultsProjected"
	EventTypeApprovalRequested    EventType = "ApprovalRequested"
	EventTypeApprovalResolved     EventType = "ApprovalResolved"
	EventTypeHookStarted          EventType = "HookStarted"
	EventTypeHookFinished         EventType = "HookFinished"
	EventTypeContextCompacted     EventType = "ContextCompacted"
	EventTypeConversationReset    EventType = "ConversationReset"
	EventTypeConversationRewound  EventType = "ConversationRewound"
	EventTypeTasksChanged         EventType = "TasksChanged"
	EventTypeMemoryChanged        EventType = "MemoryChanged"
	EventTypeToolsDiscovered      EventType = "ToolsDiscovered"
	EventTypeUsageChanged         EventType = "UsageChanged"
)

// Event is the sealed, typed fact interface used by Store.  External packages
// can construct the concrete values below but cannot add a payload type that
// the JSONL codec would silently accept.
type Event interface {
	Type() EventType
	validate() error
	seal()
}

// BlobRef points at an immutable content-addressed artifact in a session's
// blobs directory.  The store never puts blob bytes directly in an event.
type BlobRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

func (b BlobRef) validate() error {
	if len(b.Hash) != 64 {
		return fmt.Errorf("blob hash must be a 64-character SHA-256 hex string")
	}
	for _, r := range b.Hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return fmt.Errorf("blob hash contains non-hex character")
		}
	}
	if b.Size < 0 {
		return fmt.Errorf("blob size must not be negative")
	}
	return nil
}

// ContentKind and ContentBlock are the explicit message union used by model
// history.  Exactly one branch matching Kind is allowed.
type ContentKind string

const (
	ContentText       ContentKind = "text"
	ContentImage      ContentKind = "image"
	ContentToolCall   ContentKind = "tool_call"
	ContentToolResult ContentKind = "tool_result"
)

type ContentBlock struct {
	Kind ContentKind `json:"kind"`
	Text string      `json:"text,omitempty"`
	Blob *BlobRef    `json:"blob,omitempty"`
	// Path is an optional original locator for an image attachment. The blob,
	// not this path, is authoritative when rebuilding a request.
	Path       string      `json:"path,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

func (b ContentBlock) validate() error {
	switch b.Kind {
	case ContentText:
		if b.Blob != nil || b.Path != "" || b.ToolCall != nil || b.ToolResult != nil {
			return errors.New("text content block has non-text branches")
		}
	case ContentImage:
		if b.Blob == nil {
			return errors.New("image content block requires blob")
		}
		if err := b.Blob.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.ToolCall != nil || b.ToolResult != nil {
			return errors.New("image content block has unrelated branches")
		}
	case ContentToolCall:
		if b.ToolCall == nil {
			return errors.New("tool-call content block requires tool_call")
		}
		if err := b.ToolCall.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.Blob != nil || b.Path != "" || b.ToolResult != nil {
			return errors.New("tool-call content block has unrelated branches")
		}
	case ContentToolResult:
		if b.ToolResult == nil {
			return errors.New("tool-result content block requires tool_result")
		}
		if err := b.ToolResult.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.Blob != nil || b.Path != "" || b.ToolCall != nil {
			return errors.New("tool-result content block has unrelated branches")
		}
	default:
		return fmt.Errorf("unknown content kind %q", b.Kind)
	}
	return nil
}

type Message struct {
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	MessageID        string         `json:"message_id"`
	Role             string         `json:"role"`
	Content          []ContentBlock `json:"content"`
	// CreatedAt is the source timestamp for imported and newly committed
	// messages.  It is deliberately part of the typed message value instead
	// of being regenerated while replaying a log, so a projection can retain
	// legacy history timing exactly.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (m Message) validate() error {
	if err := requireID("message_id", m.MessageID); err != nil {
		return err
	}
	if m.Role == "" {
		return errors.New("message role is required")
	}
	if len(m.Content) == 0 {
		return errors.New("message content is required")
	}
	for i, block := range m.Content {
		if err := block.validate(); err != nil {
			return fmt.Errorf("message content[%d]: %w", i, err)
		}
	}
	return nil
}

type ToolCall struct {
	CallID    string          `json:"call_id"`
	ToolID    string          `json:"tool_id"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c ToolCall) validate() error {
	if err := requireID("call_id", c.CallID); err != nil {
		return err
	}
	if err := requireID("tool_id", c.ToolID); err != nil {
		return err
	}
	if len(c.Arguments) == 0 {
		return errors.New("tool arguments are required")
	}
	if !json.Valid(c.Arguments) {
		return errors.New("tool arguments are not valid JSON")
	}
	return nil
}

type ToolResult struct {
	CallID string   `json:"call_id"`
	Status string   `json:"status"`
	Text   string   `json:"text,omitempty"`
	Blob   *BlobRef `json:"blob,omitempty"`
	// CreatedAt preserves the time at which a result became part of the
	// conversation projection.  It is optional for newly emitted facts whose
	// lifecycle is represented by the surrounding transaction.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (r ToolResult) validate() error {
	if err := requireID("call_id", r.CallID); err != nil {
		return err
	}
	if r.Status == "" {
		return errors.New("tool result status is required")
	}
	if r.Blob != nil {
		return r.Blob.validate()
	}
	return nil
}

type Settings struct {
	ReasoningEffort           string   `json:"reasoning_effort,omitempty"`
	Verbosity                 string   `json:"verbosity,omitempty"`
	GenerationOptionsSet      bool     `json:"generation_options_set,omitempty"`
	Model                     string   `json:"model,omitempty"`
	Provider                  string   `json:"provider,omitempty"`
	Endpoint                  string   `json:"endpoint,omitempty"`
	Temperature               *float64 `json:"temperature,omitempty"`
	MaxOutputTokens           *int     `json:"max_output_tokens,omitempty"`
	ExecutionMode             string   `json:"execution_mode,omitempty"`
	PermissionPolicy          string   `json:"permission_policy,omitempty"`
	AlwaysAllow               []string `json:"always_allow,omitempty"`
	AlwaysDeny                []string `json:"always_deny,omitempty"`
	SandboxPolicy             string   `json:"sandbox_policy,omitempty"`
	AllowNetwork              bool     `json:"allow_network,omitempty"`
	AllowNetworkSet           bool     `json:"allow_network_set,omitempty"`
	AdditionalDirectories     []string `json:"additional_directories,omitempty"`
	DisallowedDirectories     []string `json:"disallowed_directories,omitempty"`
	ContextWindow             int      `json:"context_window,omitempty"`
	EffectiveContextWindow    int      `json:"effective_context_window,omitempty"`
	CompactThreshold          float64  `json:"compact_threshold,omitempty"`
	MaxResultSizeChars        int      `json:"max_result_size_chars,omitempty"`
	MaxTurns                  int      `json:"max_turns,omitempty"`
	MaxBudgetUSD              float64  `json:"max_budget_usd,omitempty"`
	MaxToolOutputCharsPerTurn int      `json:"max_tool_output_chars_per_turn,omitempty"`
	Workspace                 string   `json:"workspace,omitempty"`
}

func (s Settings) validate() error {
	if err := protocol.ValidateGeneration(s.ReasoningEffort, s.Verbosity); err != nil {
		return err
	}
	if s.Temperature != nil && (*s.Temperature < 0 || *s.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if s.MaxOutputTokens != nil && *s.MaxOutputTokens < 0 {
		return errors.New("max_output_tokens must not be negative")
	}
	if s.EffectiveContextWindow < 0 || s.ContextWindow < 0 || s.MaxResultSizeChars < 0 || s.MaxTurns < 0 || s.MaxToolOutputCharsPerTurn < 0 {
		return errors.New("settings limits must not be negative")
	}
	if s.MaxBudgetUSD < 0 {
		return errors.New("max_budget_usd must not be negative")
	}
	if s.CompactThreshold < 0 || s.CompactThreshold > 1 {
		return errors.New("compact_threshold must be between 0 and 1")
	}
	if s.ExecutionMode != "" && s.ExecutionMode != "execute" && s.ExecutionMode != "plan" {
		return fmt.Errorf("unknown execution_mode %q", s.ExecutionMode)
	}
	if s.PermissionPolicy != "" && s.PermissionPolicy != "default" && s.PermissionPolicy != "acceptEdits" && s.PermissionPolicy != "bypassPermissions" {
		return fmt.Errorf("unknown permission_policy %q", s.PermissionPolicy)
	}
	if s.SandboxPolicy != "" && s.SandboxPolicy != "confine" && s.SandboxPolicy != "strict" && s.SandboxPolicy != "none" {
		return fmt.Errorf("unknown sandbox_policy %q", s.SandboxPolicy)
	}
	return nil
}

type WorkflowState struct {
	Phase       string `json:"phase"`
	PlanID      string `json:"plan_id,omitempty"`
	PlanVersion uint64 `json:"plan_version,omitempty"`
}

func (w WorkflowState) validate() error {
	switch w.Phase {
	case "", "off", "drafting", "awaiting_decision":
		return nil
	default:
		return fmt.Errorf("unknown workflow phase %q", w.Phase)
	}
}

type ToolSchema struct {
	ToolID      string          `json:"tool_id"`
	Version     string          `json:"version"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Effects     []string        `json:"effects,omitempty"`
}

func (t ToolSchema) validate() error {
	if err := requireID("tool_id", t.ToolID); err != nil {
		return err
	}
	if t.Version == "" {
		return errors.New("tool version is required")
	}
	if len(t.Schema) == 0 || !json.Valid(t.Schema) {
		return errors.New("tool schema must be valid JSON")
	}
	for _, effect := range t.Effects {
		switch effect {
		case "read", "write", "network", "process", "delegate", "unknown":
		default:
			return fmt.Errorf("unknown tool effect %q", effect)
		}
	}
	return nil
}

type RequestManifest struct {
	RequestID string `json:"request_id"`
	// SessionID/TurnID/StepID and OperationID identify the owner of a frozen
	// provider call. They are optional on old manifests, but every manifest
	// emitted by the request journal carries them so a prepared request can be
	// located without guessing from a UI trace.
	SessionID      string    `json:"session_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	StepID         string    `json:"step_id,omitempty"`
	OperationID    string    `json:"operation_id,omitempty"`
	Purpose        string    `json:"purpose,omitempty"`
	Model          string    `json:"model"`
	Provider       string    `json:"provider"`
	Endpoint       string    `json:"endpoint,omitempty"`
	AdapterVersion string    `json:"adapter_version"`
	Messages       []Message `json:"messages"`
	MessageRefs    []BlobRef `json:"message_refs,omitempty"`
	// WireBody is the complete provider request body. It is deliberately
	// separate from message_refs: a loader can recognize the encoding and
	// reconstruct the exact frozen body instead of treating an opaque full-body
	// blob as a collection of message references.
	WireBody       *BlobRef     `json:"wire_body,omitempty"`
	WireFormat     string       `json:"wire_format,omitempty"`
	Tools          []ToolSchema `json:"tools,omitempty"`
	Digest         string       `json:"digest"`
	SourceRevision uint64       `json:"source_revision"`
}

func (m RequestManifest) validate() error {
	if err := requireID("request_id", m.RequestID); err != nil {
		return err
	}
	if m.Model == "" || m.Provider == "" || m.AdapterVersion == "" || m.Digest == "" {
		return errors.New("request manifest model, provider, adapter_version and digest are required")
	}
	if len(m.Messages) == 0 && len(m.MessageRefs) == 0 && m.WireBody == nil {
		return errors.New("request manifest requires messages, message_refs, or wire body")
	}
	for i, message := range m.Messages {
		if err := message.validate(); err != nil {
			return fmt.Errorf("manifest message[%d]: %w", i, err)
		}
	}
	for i, ref := range m.MessageRefs {
		if err := ref.validate(); err != nil {
			return fmt.Errorf("manifest message_ref[%d]: %w", i, err)
		}
	}
	if m.WireBody != nil {
		if err := m.WireBody.validate(); err != nil {
			return fmt.Errorf("manifest wire body: %w", err)
		}
		if m.WireFormat == "" {
			return errors.New("manifest wire_format is required when wire is present")
		}
		if !isSHA256Digest(m.Digest) || !strings.EqualFold(m.Digest, m.WireBody.Hash) {
			return errors.New("manifest digest must match wire blob hash")
		}
	}
	for i, tool := range m.Tools {
		if err := tool.validate(); err != nil {
			return fmt.Errorf("manifest tool[%d]: %w", i, err)
		}
	}
	return nil
}

type Usage struct {
	Cache        protocol.CacheStats `json:"cache,omitempty"`
	InputTokens  int64               `json:"input_tokens"`
	OutputTokens int64               `json:"output_tokens"`
	CachedTokens int64               `json:"cached_tokens,omitempty"`
	TotalTokens  int64               `json:"total_tokens"`
	Cost         float64             `json:"cost"`
	TurnCount    int64               `json:"turn_count,omitempty"`
	PriceVersion string              `json:"price_version,omitempty"`
}

func (u Usage) validate() error {
	if u.Cache.InputTokens < 0 || u.Cache.CachedTokens < 0 || u.Cache.ReportedInputTokens < 0 || u.Cache.CachedTokens > u.Cache.ReportedInputTokens || u.Cache.ReportedInputTokens > u.Cache.InputTokens {
		return errors.New("invalid cache usage values")
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedTokens < 0 || u.TotalTokens < 0 || u.Cost < 0 || u.TurnCount < 0 {
		return errors.New("usage values must not be negative")
	}
	return nil
}

type Attempt struct {
	AttemptID  string    `json:"attempt_id"`
	RequestID  string    `json:"request_id"`
	SessionID  string    `json:"session_id,omitempty"`
	TurnID     string    `json:"turn_id,omitempty"`
	StepID     string    `json:"step_id,omitempty"`
	Purpose    string    `json:"purpose,omitempty"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Endpoint   string    `json:"endpoint,omitempty"`
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error,omitempty"`
	Usage      Usage     `json:"usage"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func (a Attempt) validate() error {
	if err := requireID("attempt_id", a.AttemptID); err != nil {
		return err
	}
	if err := requireID("request_id", a.RequestID); err != nil {
		return err
	}
	if a.Provider == "" || a.Model == "" || a.Outcome == "" {
		return errors.New("attempt provider, model and outcome are required")
	}
	if err := validateOutcome(a.Outcome); err != nil {
		return err
	}
	return a.Usage.validate()
}

type Budget struct {
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Truncated bool  `json:"truncated"`
}

func (b Budget) validate() error {
	if b.Limit < 0 || b.Used < 0 {
		return errors.New("budget values must not be negative")
	}
	return nil
}

type ProjectedToolResult struct {
	CallID string `json:"call_id"`
	Status string `json:"status"`
	Text   string `json:"text,omitempty"`
}

func (r ProjectedToolResult) validate() error {
	if err := requireID("call_id", r.CallID); err != nil {
		return err
	}
	if r.Status == "" {
		return errors.New("projected tool result status is required")
	}
	return nil
}

type Task struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func (t Task) validate() error {
	if err := requireID("task_id", t.TaskID); err != nil {
		return err
	}
	if t.Title == "" || t.Status == "" {
		return errors.New("task title and status are required")
	}
	return nil
}

type ApprovalRequest struct {
	ApprovalID     string `json:"approval_id"`
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id,omitempty"`
	CallID         string `json:"call_id,omitempty"`
	PlanID         string `json:"plan_id,omitempty"`
	PlanVersion    uint64 `json:"plan_version,omitempty"`
	ArgumentDigest string `json:"argument_digest"`
	Workspace      string `json:"workspace"`
	ToolVersion    string `json:"tool_version,omitempty"`
	PolicyRevision uint64 `json:"policy_revision"`
}

func (a ApprovalRequest) validate() error {
	if err := requireID("approval_id", a.ApprovalID); err != nil {
		return err
	}
	if err := requireID("session_id", a.SessionID); err != nil {
		return err
	}
	if a.ArgumentDigest == "" {
		return errors.New("approval argument_digest is required")
	}
	return nil
}

type ApprovalResolution struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
}

func (a ApprovalResolution) validate() error {
	if err := requireID("approval_id", a.ApprovalID); err != nil {
		return err
	}
	switch a.Decision {
	case "allow", "deny", "cancel", "expired":
		return nil
	default:
		return fmt.Errorf("unknown approval decision %q", a.Decision)
	}
}

// Lifecycle and conversation events.
type SessionCreated struct {
	SessionID     string    `json:"session_id"`
	FormatVersion uint32    `json:"format_version"`
	Source        string    `json:"source,omitempty"`
	ParentID      string    `json:"parent_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type SessionImported struct {
	SessionID     string    `json:"session_id"`
	FormatVersion uint32    `json:"format_version"`
	Source        string    `json:"source"`
	OriginalPath  string    `json:"original_path,omitempty"`
	ImportedAt    time.Time `json:"imported_at"`
	ParentID      string    `json:"parent_id,omitempty"`
	BranchPoint   int       `json:"branch_point,omitempty"`
	BranchSummary string    `json:"branch_summary,omitempty"`
}

type SessionClosed struct {
	SessionID string    `json:"session_id"`
	Outcome   string    `json:"outcome"`
	ClosedAt  time.Time `json:"closed_at"`
}

type InputQueued struct {
	// InputDigest retains the original typed-input identity when clipboard
	// images are lowered to the existing frozen Markdown/blob representation.
	InputDigest  string    `json:"input_digest,omitempty"`
	InputID      string    `json:"input_id"`
	MessageID    string    `json:"message_id,omitempty"`
	Text         string    `json:"text,omitempty"`
	Blob         *BlobRef  `json:"blob,omitempty"`
	Attachments  []BlobRef `json:"attachments,omitempty"`
	ImagesFrozen bool      `json:"images_frozen,omitempty"`
	Strategy     string    `json:"strategy"`
	TurnID       string    `json:"turn_id,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

type InputDelivered struct {
	InputID string `json:"input_id"`
	TurnID  string `json:"turn_id"`
}

type InputCancelled struct {
	InputID string `json:"input_id"`
	Reason  string `json:"reason,omitempty"`
}

// CommandScheduled is the durable admission fact for an asynchronous
// command. InputDigest is the SHA-256 of the canonical command JSON, so a
// reused CommandID with different arguments is rejected by event-id
// deduplication without persisting command payloads twice.
type CommandScheduled struct {
	CommandID      string `json:"command_id"`
	OperationID    string `json:"operation_id,omitempty"`
	Name           string `json:"name"`
	InputDigest    string `json:"input_digest"`
	SourceRevision uint64 `json:"source_revision,omitempty"`
}

type CommandCompleted struct {
	CommandID string   `json:"command_id"`
	Outcome   string   `json:"outcome"`
	Code      string   `json:"code,omitempty"`
	Report    string   `json:"report,omitempty"`
	Output    *BlobRef `json:"output,omitempty"`
}

type SettingsChanged struct {
	Revision uint64   `json:"revision"`
	Settings Settings `json:"settings"`
}

type WorkflowChanged struct {
	Workflow WorkflowState `json:"workflow"`
}

type TurnStarted struct {
	TurnID      string   `json:"turn_id"`
	StepID      string   `json:"step_id"`
	InputIDs    []string `json:"input_ids,omitempty"`
	ContextRev  uint64   `json:"context_revision"`
	SettingsRev uint64   `json:"settings_revision"`
}

type TurnFinished struct {
	TurnID  string `json:"turn_id"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type RequestPrepared struct {
	Manifest RequestManifest `json:"manifest"`
}

type AttemptFinished struct {
	Attempt Attempt `json:"attempt"`
}

type AssistantCommitted struct {
	TurnID    string     `json:"turn_id"`
	StepID    string     `json:"step_id,omitempty"`
	Message   Message    `json:"message"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ToolStarted struct {
	TurnID string `json:"turn_id"`
	// StepID and CallIndex make the original model step/call order explicit.
	// They are optional for old logs, whose turn/call identity remains valid.
	StepID         string   `json:"step_id,omitempty"`
	CallIndex      int      `json:"call_index,omitempty"`
	Call           ToolCall `json:"call"`
	Fingerprint    string   `json:"fingerprint"`
	PolicyRevision uint64   `json:"policy_revision"`
}

type ToolFinished struct {
	TurnID     string     `json:"turn_id"`
	StepID     string     `json:"step_id,omitempty"`
	CallID     string     `json:"call_id"`
	EffectID   string     `json:"effect_id,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	SideEffect string     `json:"side_effect,omitempty"`
	RawOutput  *BlobRef   `json:"raw_output,omitempty"`
	Result     ToolResult `json:"result"`
}

type ToolResultsProjected struct {
	TurnID  string                `json:"turn_id"`
	StepID  string                `json:"step_id,omitempty"`
	Results []ProjectedToolResult `json:"results"`
	Budget  Budget                `json:"budget"`
}

type ApprovalRequested struct {
	Request ApprovalRequest `json:"request"`
}

type ApprovalResolved struct {
	Resolution ApprovalResolution `json:"resolution"`
}

type HookStarted struct {
	EffectID string `json:"effect_id"`
	HookID   string `json:"hook_id"`
	Stage    string `json:"stage"`
}

type HookFinished struct {
	EffectID string   `json:"effect_id"`
	HookID   string   `json:"hook_id"`
	Stage    string   `json:"stage"`
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
	Output   *BlobRef `json:"output,omitempty"`
}

type ContextCompacted struct {
	BaseSeq        uint64   `json:"base_seq"`
	Summary        Message  `json:"summary"`
	KeptMessageIDs []string `json:"kept_message_ids"`
	SourceRevision uint64   `json:"source_revision"`
}

type ConversationReset struct {
	ResetID string `json:"reset_id"`
	Reason  string `json:"reason,omitempty"`
}

type ConversationRewound struct {
	RewindID string `json:"rewind_id"`
	ToSeq    uint64 `json:"to_seq"`
	Reason   string `json:"reason,omitempty"`
}

type TasksChanged struct {
	Revision uint64 `json:"revision"`
	Tasks    []Task `json:"tasks"`
}

type MemoryChanged struct {
	Revision uint64   `json:"revision"`
	Text     string   `json:"text,omitempty"`
	Blob     *BlobRef `json:"blob,omitempty"`
	Cleared  bool     `json:"cleared,omitempty"`
}

type ToolsDiscovered struct {
	CatalogVersion uint64       `json:"catalog_version"`
	Tools          []ToolSchema `json:"tools"`
}

// UsageChanged records an aggregate usage projection when an older session
// only has totals rather than request-level attempt facts.  New runtimes
// should prefer AttemptFinished for per-request accounting; this event keeps
// legacy import observable without inventing manifests or attempts.
type UsageChanged struct {
	Revision uint64 `json:"revision"`
	Usage    Usage  `json:"usage"`
}

func requireID(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a newline", name)
	}
	return nil
}

func isSHA256Digest(value string) bool {
	if len(value) != sha256HexLength {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validateTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func validateOutcome(value string) error {
	switch value {
	case "success", "error", "cancelled", "interrupted", "unknown", "denied", "queued", "applied", "rejected":
		return nil
	default:
		return fmt.Errorf("unknown outcome %q", value)
	}
}

func (e SessionCreated) Type() EventType       { return EventTypeSessionCreated }
func (e SessionCreated) seal()                 {}
func (e SessionImported) Type() EventType      { return EventTypeSessionImported }
func (e SessionImported) seal()                {}
func (e SessionClosed) Type() EventType        { return EventTypeSessionClosed }
func (e SessionClosed) seal()                  {}
func (e InputQueued) Type() EventType          { return EventTypeInputQueued }
func (e InputQueued) seal()                    {}
func (e InputDelivered) Type() EventType       { return EventTypeInputDelivered }
func (e InputDelivered) seal()                 {}
func (e InputCancelled) Type() EventType       { return EventTypeInputCancelled }
func (e InputCancelled) seal()                 {}
func (e CommandScheduled) Type() EventType     { return EventTypeCommandScheduled }
func (e CommandScheduled) seal()               {}
func (e CommandCompleted) Type() EventType     { return EventTypeCommandCompleted }
func (e CommandCompleted) seal()               {}
func (e SettingsChanged) Type() EventType      { return EventTypeSettingsChanged }
func (e SettingsChanged) seal()                {}
func (e WorkflowChanged) Type() EventType      { return EventTypeWorkflowChanged }
func (e WorkflowChanged) seal()                {}
func (e TurnStarted) Type() EventType          { return EventTypeTurnStarted }
func (e TurnStarted) seal()                    {}
func (e TurnFinished) Type() EventType         { return EventTypeTurnFinished }
func (e TurnFinished) seal()                   {}
func (e RequestPrepared) Type() EventType      { return EventTypeRequestPrepared }
func (e RequestPrepared) seal()                {}
func (e AttemptFinished) Type() EventType      { return EventTypeAttemptFinished }
func (e AttemptFinished) seal()                {}
func (e AssistantCommitted) Type() EventType   { return EventTypeAssistantCommitted }
func (e AssistantCommitted) seal()             {}
func (e ToolStarted) Type() EventType          { return EventTypeToolStarted }
func (e ToolStarted) seal()                    {}
func (e ToolFinished) Type() EventType         { return EventTypeToolFinished }
func (e ToolFinished) seal()                   {}
func (e ToolResultsProjected) Type() EventType { return EventTypeToolResultsProjected }
func (e ToolResultsProjected) seal()           {}
func (e ApprovalRequested) Type() EventType    { return EventTypeApprovalRequested }
func (e ApprovalRequested) seal()              {}
func (e ApprovalResolved) Type() EventType     { return EventTypeApprovalResolved }
func (e ApprovalResolved) seal()               {}
func (e HookStarted) Type() EventType          { return EventTypeHookStarted }
func (e HookStarted) seal()                    {}
func (e HookFinished) Type() EventType         { return EventTypeHookFinished }
func (e HookFinished) seal()                   {}
func (e ContextCompacted) Type() EventType     { return EventTypeContextCompacted }
func (e ContextCompacted) seal()               {}
func (e ConversationReset) Type() EventType    { return EventTypeConversationReset }
func (e ConversationReset) seal()              {}
func (e ConversationRewound) Type() EventType  { return EventTypeConversationRewound }
func (e ConversationRewound) seal()            {}
func (e TasksChanged) Type() EventType         { return EventTypeTasksChanged }
func (e TasksChanged) seal()                   {}
func (e MemoryChanged) Type() EventType        { return EventTypeMemoryChanged }
func (e MemoryChanged) seal()                  {}
func (e ToolsDiscovered) Type() EventType      { return EventTypeToolsDiscovered }
func (e ToolsDiscovered) seal()                {}
func (e UsageChanged) Type() EventType         { return EventTypeUsageChanged }
func (e UsageChanged) seal()                   {}

func (e SessionCreated) validate() error {
	if err := requireID("session_id", e.SessionID); err != nil {
		return err
	}
	if e.FormatVersion == 0 {
		return errors.New("format_version is required")
	}
	if e.ParentID != "" {
		if err := requireID("parent_id", e.ParentID); err != nil {
			return err
		}
	}
	return validateTime("created_at", e.CreatedAt)
}
func (e SessionImported) validate() error {
	if err := requireID("session_id", e.SessionID); err != nil {
		return err
	}
	if e.FormatVersion == 0 || e.Source == "" {
		return errors.New("format_version and source are required")
	}
	return validateTime("imported_at", e.ImportedAt)
}
func (e SessionClosed) validate() error {
	if err := requireID("session_id", e.SessionID); err != nil {
		return err
	}
	if err := validateOutcome(e.Outcome); err != nil {
		return err
	}
	return validateTime("closed_at", e.ClosedAt)
}
func (e InputQueued) validate() error {
	if err := requireID("input_id", e.InputID); err != nil {
		return err
	}
	if e.InputDigest != "" && !isSHA256Digest(e.InputDigest) {
		return errors.New("input_digest must be a SHA-256 hex digest")
	}
	if e.Text == "" && e.Blob == nil {
		return errors.New("input requires text or blob")
	}
	if e.Blob != nil {
		if err := e.Blob.validate(); err != nil {
			return err
		}
	}
	for i, attachment := range e.Attachments {
		if err := attachment.validate(); err != nil {
			return fmt.Errorf("attachments[%d]: %w", i, err)
		}
	}
	if e.Strategy != "steer" && e.Strategy != "followup" {
		return fmt.Errorf("unknown input strategy %q", e.Strategy)
	}
	return nil
}
func (e InputDelivered) validate() error {
	if err := requireID("input_id", e.InputID); err != nil {
		return err
	}
	return requireID("turn_id", e.TurnID)
}
func (e InputCancelled) validate() error { return requireID("input_id", e.InputID) }
func (e CommandScheduled) validate() error {
	if err := requireID("command_id", e.CommandID); err != nil {
		return err
	}
	if e.OperationID != "" {
		if err := requireID("operation_id", e.OperationID); err != nil {
			return err
		}
	}
	if err := requireID("name", e.Name); err != nil {
		return err
	}
	if !isSHA256Digest(e.InputDigest) {
		return errors.New("command input_digest must be a SHA-256 hex digest")
	}
	return nil
}
func (e CommandCompleted) validate() error {
	if err := requireID("command_id", e.CommandID); err != nil {
		return err
	}
	if err := validateOutcome(e.Outcome); err != nil {
		return err
	}
	if e.Output != nil {
		return e.Output.validate()
	}
	return nil
}
func (e SettingsChanged) validate() error { return e.Settings.validate() }
func (e WorkflowChanged) validate() error { return e.Workflow.validate() }
func (e TurnStarted) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	return requireID("step_id", e.StepID)
}
func (e TurnFinished) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	return validateOutcome(e.Outcome)
}
func (e RequestPrepared) validate() error { return e.Manifest.validate() }
func (e AttemptFinished) validate() error { return e.Attempt.validate() }
func (e AssistantCommitted) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	if err := e.Message.validate(); err != nil {
		return err
	}
	for i, call := range e.ToolCalls {
		if err := call.validate(); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
	}
	return nil
}
func (e ToolStarted) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	if err := e.Call.validate(); err != nil {
		return err
	}
	if e.CallIndex < 0 {
		return errors.New("tool call index must not be negative")
	}
	return requireID("fingerprint", e.Fingerprint)
}
func (e ToolFinished) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	if err := requireID("call_id", e.CallID); err != nil {
		return err
	}
	if e.Status == "" {
		return errors.New("tool status is required")
	}
	if e.RawOutput != nil {
		if err := e.RawOutput.validate(); err != nil {
			return err
		}
	}
	if err := e.Result.validate(); err != nil {
		return err
	}
	if e.Result.CallID != e.CallID {
		return errors.New("tool result call_id does not match call_id")
	}
	return nil
}
func (e ToolResultsProjected) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	if len(e.Results) == 0 {
		return errors.New("tool results are required")
	}
	for i, r := range e.Results {
		if err := r.validate(); err != nil {
			return fmt.Errorf("results[%d]: %w", i, err)
		}
	}
	return e.Budget.validate()
}
func (e ApprovalRequested) validate() error { return e.Request.validate() }
func (e ApprovalResolved) validate() error  { return e.Resolution.validate() }
func (e HookStarted) validate() error {
	if err := requireID("effect_id", e.EffectID); err != nil {
		return err
	}
	if err := requireID("hook_id", e.HookID); err != nil {
		return err
	}
	return requireID("stage", e.Stage)
}
func (e HookFinished) validate() error {
	if err := requireID("effect_id", e.EffectID); err != nil {
		return err
	}
	if err := requireID("hook_id", e.HookID); err != nil {
		return err
	}
	if e.Stage == "" || e.Status == "" {
		return errors.New("hook stage and status are required")
	}
	if e.Output != nil {
		return e.Output.validate()
	}
	return nil
}
func (e ContextCompacted) validate() error {
	if err := e.Summary.validate(); err != nil {
		return err
	}
	if len(e.KeptMessageIDs) == 0 {
		return errors.New("kept_message_ids are required")
	}
	for _, id := range e.KeptMessageIDs {
		if err := requireID("kept_message_id", id); err != nil {
			return err
		}
	}
	return nil
}
func (e ConversationReset) validate() error { return requireID("reset_id", e.ResetID) }
func (e ConversationRewound) validate() error {
	return requireID("rewind_id", e.RewindID)
}
func (e TasksChanged) validate() error {
	for i, task := range e.Tasks {
		if err := task.validate(); err != nil {
			return fmt.Errorf("tasks[%d]: %w", i, err)
		}
	}
	return nil
}
func (e MemoryChanged) validate() error {
	if e.Text == "" && e.Blob == nil && !e.Cleared {
		return errors.New("memory requires text or blob")
	}
	if e.Blob != nil {
		return e.Blob.validate()
	}
	return nil
}
func (e ToolsDiscovered) validate() error {
	if e.CatalogVersion == 0 {
		return errors.New("catalog_version is required")
	}
	for i, tool := range e.Tools {
		if err := tool.validate(); err != nil {
			return fmt.Errorf("tools[%d]: %w", i, err)
		}
	}
	return nil
}
func (e UsageChanged) validate() error { return e.Usage.validate() }

type wireEvent struct {
	Type    EventType       `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func encodeEvent(event Event) (wireEvent, error) {
	if event == nil {
		return wireEvent{}, errors.New("nil event")
	}
	value := reflect.ValueOf(event)
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return wireEvent{}, fmt.Errorf("nil event type %T", event)
	}
	if err := event.validate(); err != nil {
		return wireEvent{}, fmt.Errorf("%s: %w", event.Type(), err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return wireEvent{}, fmt.Errorf("encode %s: %w", event.Type(), err)
	}
	return wireEvent{Type: event.Type(), Payload: data}, nil
}

func decodeEvent(w wireEvent) (Event, error) {
	if w.Type == "" || len(w.Payload) == 0 {
		return nil, errors.New("event type and payload are required")
	}
	var event Event
	switch w.Type {
	case EventTypeChildRunRecorded:
		event = &ChildRunRecorded{}
	case EventTypeSessionCreated:
		event = &SessionCreated{}
	case EventTypeSessionImported:
		event = &SessionImported{}
	case EventTypeSessionClosed:
		event = &SessionClosed{}
	case EventTypeInputQueued:
		event = &InputQueued{}
	case EventTypeInputDelivered:
		event = &InputDelivered{}
	case EventTypeInputCancelled:
		event = &InputCancelled{}
	case EventTypeCommandScheduled:
		event = &CommandScheduled{}
	case EventTypeCommandCompleted:
		event = &CommandCompleted{}
	case EventTypeSettingsChanged:
		event = &SettingsChanged{}
	case EventTypeWorkflowChanged:
		event = &WorkflowChanged{}
	case EventTypeTurnStarted:
		event = &TurnStarted{}
	case EventTypeTurnFinished:
		event = &TurnFinished{}
	case EventTypeRequestPrepared:
		event = &RequestPrepared{}
	case EventTypeAttemptFinished:
		event = &AttemptFinished{}
	case EventTypeAssistantCommitted:
		event = &AssistantCommitted{}
	case EventTypeToolStarted:
		event = &ToolStarted{}
	case EventTypeToolFinished:
		event = &ToolFinished{}
	case EventTypeToolResultsProjected:
		event = &ToolResultsProjected{}
	case EventTypeApprovalRequested:
		event = &ApprovalRequested{}
	case EventTypeApprovalResolved:
		event = &ApprovalResolved{}
	case EventTypeHookStarted:
		event = &HookStarted{}
	case EventTypeHookFinished:
		event = &HookFinished{}
	case EventTypeContextCompacted:
		event = &ContextCompacted{}
	case EventTypeConversationReset:
		event = &ConversationReset{}
	case EventTypeConversationRewound:
		event = &ConversationRewound{}
	case EventTypeTasksChanged:
		event = &TasksChanged{}
	case EventTypeMemoryChanged:
		event = &MemoryChanged{}
	case EventTypeToolsDiscovered:
		event = &ToolsDiscovered{}
	case EventTypeUsageChanged:
		event = &UsageChanged{}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEventType, w.Type)
	}
	if err := decodeStrict(w.Payload, event); err != nil {
		return nil, fmt.Errorf("decode %s: %w", w.Type, err)
	}
	if err := event.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", w.Type, err)
	}
	return event, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func normalizeEvent(event Event) (Event, wireEvent, error) {
	w, err := encodeEvent(event)
	if err != nil {
		return nil, wireEvent{}, err
	}
	decoded, err := decodeEvent(w)
	if err != nil {
		return nil, wireEvent{}, err
	}
	return decoded, w, nil
}
