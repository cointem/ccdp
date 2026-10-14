package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

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

type ApprovalRequest struct {
	ApprovalID             string                       `json:"approval_id"`
	SessionID              string                       `json:"session_id"`
	TurnID                 string                       `json:"turn_id,omitempty"`
	CallID                 string                       `json:"call_id,omitempty"`
	PlanID                 string                       `json:"plan_id,omitempty"`
	PlanVersion            uint64                       `json:"plan_version,omitempty"`
	ArgumentDigest         string                       `json:"argument_digest"`
	BusinessArgumentDigest string                       `json:"business_argument_digest"`
	Workspace              string                       `json:"workspace"`
	ToolVersion            string                       `json:"tool_version,omitempty"`
	PolicyRevision         uint64                       `json:"policy_revision"`
	Capabilities           []protocol.CapabilityRequest `json:"capabilities,omitempty"`
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
	if a.BusinessArgumentDigest == "" {
		return errors.New("approval business_argument_digest is required")
	}
	return nil
}

type ApprovalResolution struct {
	ApprovalID      string                       `json:"approval_id"`
	Decision        string                       `json:"decision"`
	Reason          string                       `json:"reason,omitempty"`
	ResolvedBy      string                       `json:"resolved_by,omitempty"`
	CapabilityScope protocol.CapabilityScope     `json:"capability_scope,omitempty"`
	Capabilities    []protocol.CapabilityRequest `json:"capabilities,omitempty"`
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

type ToolsDiscovered struct {
	CatalogVersion uint64       `json:"catalog_version"`
	Tools          []ToolSchema `json:"tools"`
}

func (e ToolStarted) Type() EventType { return EventTypeToolStarted }

func (e ToolStarted) seal() {}

func (e ToolFinished) Type() EventType { return EventTypeToolFinished }

func (e ToolFinished) seal() {}

func (e ToolResultsProjected) Type() EventType { return EventTypeToolResultsProjected }

func (e ToolResultsProjected) seal() {}

func (e ApprovalRequested) Type() EventType { return EventTypeApprovalRequested }

func (e ApprovalRequested) seal() {}

func (e ApprovalResolved) Type() EventType { return EventTypeApprovalResolved }

func (e ApprovalResolved) seal() {}

func (e HookStarted) Type() EventType { return EventTypeHookStarted }

func (e HookStarted) seal() {}

func (e HookFinished) Type() EventType { return EventTypeHookFinished }

func (e HookFinished) seal() {}

func (e ToolsDiscovered) Type() EventType { return EventTypeToolsDiscovered }

func (e ToolsDiscovered) seal() {}

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

func (e ApprovalResolved) validate() error { return e.Resolution.validate() }

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
