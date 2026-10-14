package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

type Settings struct {
	ReasoningEffort               string   `json:"reasoning_effort,omitempty"`
	Verbosity                     string   `json:"verbosity,omitempty"`
	GenerationOptionsSet          bool     `json:"generation_options_set,omitempty"`
	Model                         string   `json:"model,omitempty"`
	Provider                      string   `json:"provider,omitempty"`
	Endpoint                      string   `json:"endpoint,omitempty"`
	Temperature                   *float64 `json:"temperature,omitempty"`
	MaxOutputTokens               *int     `json:"max_output_tokens,omitempty"`
	ExecutionMode                 string   `json:"execution_mode,omitempty"`
	PermissionPolicy              string   `json:"permission_policy,omitempty"`
	AlwaysAllow                   []string `json:"always_allow,omitempty"`
	AlwaysDeny                    []string `json:"always_deny,omitempty"`
	NetworkAccess                 bool     `json:"network_access,omitempty"`
	AdditionalDirectories         []string `json:"additional_directories,omitempty"`
	AdditionalReadOnlyDirectories []string `json:"additional_read_only_directories,omitempty"`
	DisallowedDirectories         []string `json:"disallowed_directories,omitempty"`
	ContextWindow                 int      `json:"context_window,omitempty"`
	EffectiveContextWindow        int      `json:"effective_context_window,omitempty"`
	CompactThreshold              float64  `json:"compact_threshold,omitempty"`
	MaxResultSizeChars            int      `json:"max_result_size_chars,omitempty"`
	MaxTurns                      int      `json:"max_turns,omitempty"`
	MaxToolOutputCharsPerTurn     int      `json:"max_tool_output_chars_per_turn,omitempty"`
	Workspace                     string   `json:"workspace,omitempty"`
}

func (s *Settings) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, removed := range []string{"sandbox_policy", "allow_network", "allow_network_set"} {
		if _, exists := fields[removed]; exists {
			return fmt.Errorf("session: setting %q is no longer supported", removed)
		}
	}
	type wire Settings
	return json.Unmarshal(data, (*wire)(s))
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
	if s.CompactThreshold < 0 || s.CompactThreshold > 1 {
		return errors.New("compact_threshold must be between 0 and 1")
	}
	if s.ExecutionMode != "" && s.ExecutionMode != "execute" && s.ExecutionMode != "plan" {
		return fmt.Errorf("unknown execution_mode %q", s.ExecutionMode)
	}
	if s.PermissionPolicy != "" && s.PermissionPolicy != "default" && s.PermissionPolicy != "acceptEdits" && s.PermissionPolicy != "bypassPermissions" {
		return fmt.Errorf("unknown permission_policy %q", s.PermissionPolicy)
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
	TurnID     string `json:"turn_id"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

func (e SessionCreated) Type() EventType { return EventTypeSessionCreated }

func (e SessionCreated) seal() {}

func (e SessionImported) Type() EventType { return EventTypeSessionImported }

func (e SessionImported) seal() {}

func (e SessionClosed) Type() EventType { return EventTypeSessionClosed }

func (e SessionClosed) seal() {}

func (e InputQueued) Type() EventType { return EventTypeInputQueued }

func (e InputQueued) seal() {}

func (e InputDelivered) Type() EventType { return EventTypeInputDelivered }

func (e InputDelivered) seal() {}

func (e InputCancelled) Type() EventType { return EventTypeInputCancelled }

func (e InputCancelled) seal() {}

func (e CommandScheduled) Type() EventType { return EventTypeCommandScheduled }

func (e CommandScheduled) seal() {}

func (e CommandCompleted) Type() EventType { return EventTypeCommandCompleted }

func (e CommandCompleted) seal() {}

func (e SettingsChanged) Type() EventType { return EventTypeSettingsChanged }

func (e SettingsChanged) seal() {}

func (e WorkflowChanged) Type() EventType { return EventTypeWorkflowChanged }

func (e WorkflowChanged) seal() {}

func (e TurnStarted) Type() EventType { return EventTypeTurnStarted }

func (e TurnStarted) seal() {}

func (e TurnFinished) Type() EventType { return EventTypeTurnFinished }

func (e TurnFinished) seal() {}

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
	if e.Text == "" && e.Blob == nil && len(e.Attachments) == 0 {
		return errors.New("input requires text, blob or attachments")
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
	if e.Strategy != "steer" && e.Strategy != "followup" && e.Strategy != "message" {
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
	if e.DurationMs < 0 {
		return errors.New("duration_ms must not be negative")
	}
	return validateOutcome(e.Outcome)
}
