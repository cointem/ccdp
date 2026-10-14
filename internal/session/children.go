package session

import (
	"errors"

	"ccdp/internal/protocol"
)

// ChildRunRecorded is log-only metadata, never part of model history. Each
// change is written by the owning session; terminal copies may also be kept
// by the child as a durable outbox entry for recovery.
type ChildRunRecorded struct {
	GoalSessionID   string                 `json:"goal_session_id,omitempty"`
	GoalID          string                 `json:"goal_id,omitempty"`
	PendingMessages []protocol.InputView   `json:"pending_messages,omitempty"`
	Version         int                    `json:"version"`
	Child           protocol.ChildSession  `json:"child"`
	SystemPrompt    string                 `json:"system_prompt,omitempty"`
	DeliveryID      string                 `json:"delivery_id,omitempty"`
	Delivered       bool                   `json:"delivered,omitempty"`
	UsageAccounted  bool                   `json:"usage_accounted,omitempty"`
	CommandID       string                 `json:"command_id,omitempty"`
	CommandDigest   string                 `json:"command_digest,omitempty"`
	UsageBaseline   protocol.UsageSnapshot `json:"usage_baseline,omitempty"`
}

const EventTypeChildRunRecorded EventType = "ChildRunRecorded"

func (ChildRunRecorded) Type() EventType { return EventTypeChildRunRecorded }

func (ChildRunRecorded) seal() {}

func (e ChildRunRecorded) validate() error {
	if (e.GoalSessionID == "") != (e.GoalID == "") {
		return errors.New("child goal binding requires session id and goal id")
	}
	if e.GoalID != "" {
		if err := validateSessionID(e.GoalSessionID); err != nil {
			return err
		}
		if err := requireID("goal_id", e.GoalID); err != nil {
			return err
		}
	}
	if e.Version != 1 {
		return errors.New("unsupported child run version")
	}
	if e.Child.SessionID == "" || e.Child.ParentSessionID == "" || e.Child.Run.ID == "" || e.Child.DelegationID == "" {
		return errors.New("child run identity is required")
	}
	for _, id := range []string{string(e.Child.SessionID), string(e.Child.ParentSessionID), string(e.Child.RootSessionID)} {
		if err := validateSessionID(id); err != nil {
			return err
		}
	}
	if e.Child.Role != "" && e.Child.Role != "explorer" && e.Child.Role != "worker" {
		return errors.New("invalid child role")
	}
	if e.Child.WorkspaceMode != "" && e.Child.WorkspaceMode != "shared" && e.Child.WorkspaceMode != "isolated" {
		return errors.New("invalid child workspace mode")
	}
	if e.Child.WorkspaceMode == "isolated" && (e.Child.Workspace == "" || len(e.Child.BaselineID) != 64) {
		return errors.New("write child requires an isolated workspace and baseline")
	}
	if e.Child.Purpose != "task" && e.Child.Purpose != "guardian" && e.Child.Purpose != "review" {
		return errors.New("invalid child purpose")
	}
	switch e.Child.Run.Status {
	case "queued", "starting", "running", "waiting_approval", "waiting_question", "waiting_plan", "stopping", "settling", "succeeded", "partial", "failed", "cancelled", "interrupted", "unknown":
	default:
		return errors.New("invalid child run status")
	}
	if e.Child.Run.WaitPolicy != "join" && e.Child.Run.WaitPolicy != "notify" && e.Child.Run.WaitPolicy != "observe" {
		return errors.New("invalid child wait policy")
	}
	return nil
}
