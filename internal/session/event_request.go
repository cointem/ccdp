package session

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"ccdp/internal/protocol"
)

type RequestManifest struct {
	// ReservedUsage is the frozen settlement upper bound for a request whose
	// process ends before AttemptFinished. It contains no cache credit.
	ReservedUsage *Usage `json:"reserved_usage,omitempty"`
	GoalSessionID string `json:"goal_session_id,omitempty"`
	GoalID        string `json:"goal_id,omitempty"`
	RequestID     string `json:"request_id"`
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
	if m.ReservedUsage != nil {
		if err := m.ReservedUsage.validate(); err != nil {
			return fmt.Errorf("reserved usage: %w", err)
		}
		if m.ReservedUsage.InputTokens > math.MaxInt64-m.ReservedUsage.OutputTokens || m.ReservedUsage.TotalTokens != m.ReservedUsage.InputTokens+m.ReservedUsage.OutputTokens || m.ReservedUsage.CachedTokens != 0 {
			return errors.New("reserved usage requires the full input/output token total without cache credit")
		}
	}
	if (m.GoalSessionID == "") != (m.GoalID == "") {
		return errors.New("goal request binding requires both session id and goal id")
	}
	if m.GoalID != "" {
		if err := requireID("goal_session_id", m.GoalSessionID); err != nil {
			return err
		}
		if err := requireID("goal_id", m.GoalID); err != nil {
			return err
		}
	}
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
	TurnCount    int64               `json:"turn_count,omitempty"`
}

func (u Usage) validate() error {
	if u.Cache.InputTokens < 0 || u.Cache.CachedTokens < 0 || u.Cache.ReportedInputTokens < 0 || u.Cache.CachedTokens > u.Cache.ReportedInputTokens || u.Cache.ReportedInputTokens > u.Cache.InputTokens {
		return errors.New("invalid cache usage values")
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedTokens < 0 || u.TotalTokens < 0 || u.TurnCount < 0 {
		return errors.New("usage values must not be negative")
	}
	return nil
}

type Attempt struct {
	UsageEstimated      bool      `json:"usage_estimated,omitempty"`
	GoalTokens          int64     `json:"goal_tokens,omitempty"`
	GoalTokensEstimated bool      `json:"goal_tokens_estimated,omitempty"`
	AttemptID           string    `json:"attempt_id"`
	RequestID           string    `json:"request_id"`
	SessionID           string    `json:"session_id,omitempty"`
	TurnID              string    `json:"turn_id,omitempty"`
	StepID              string    `json:"step_id,omitempty"`
	Purpose             string    `json:"purpose,omitempty"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Endpoint            string    `json:"endpoint,omitempty"`
	Outcome             string    `json:"outcome"`
	Error               string    `json:"error,omitempty"`
	Usage               Usage     `json:"usage"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
}

func (a Attempt) validate() error {
	if a.GoalTokens < 0 {
		return errors.New("goal tokens must not be negative")
	}
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

type RequestPrepared struct {
	Manifest RequestManifest `json:"manifest"`
}

type AttemptFinished struct {
	Attempt Attempt `json:"attempt"`
}

// UsageChanged records aggregate usage for snapshots without request-level
// facts, including in-memory resume. Live requests use AttemptFinished.
type UsageChanged struct {
	SourceSessionID string `json:"source_session_id,omitempty"`
	SourceAttemptID string `json:"source_attempt_id,omitempty"`
	Revision        uint64 `json:"revision"`
	Usage           Usage  `json:"usage"`
}

func (e RequestPrepared) Type() EventType { return EventTypeRequestPrepared }

func (e RequestPrepared) seal() {}

func (e AttemptFinished) Type() EventType { return EventTypeAttemptFinished }

func (e AttemptFinished) seal() {}

func (e UsageChanged) Type() EventType { return EventTypeUsageChanged }

func (e UsageChanged) seal() {}

func (e RequestPrepared) validate() error { return e.Manifest.validate() }

func (e AttemptFinished) validate() error { return e.Attempt.validate() }

func (e UsageChanged) validate() error { return e.Usage.validate() }
