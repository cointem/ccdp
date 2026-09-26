package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type RunID string
type DelegationID string

// AgentCapabilities are presentation hints. The owner validates every command.
type AgentCapabilities struct {
	Observe   bool `json:"observe"`
	Send      bool `json:"send"`
	Interrupt bool `json:"interrupt"`
	Cancel    bool `json:"cancel"`
	Continue  bool `json:"continue"`
	Approve   bool `json:"approve"`
}

type ChildSession struct {
	SessionID       SessionID         `json:"session_id"`
	RootSessionID   SessionID         `json:"root_session_id"`
	ParentSessionID SessionID         `json:"parent_session_id"`
	DelegationID    DelegationID      `json:"delegation_id"`
	ParentTurnID    TurnID            `json:"parent_turn_id,omitempty"`
	ParentCallID    CallID            `json:"parent_call_id,omitempty"`
	BatchIndex      int               `json:"batch_index"`
	Title           string            `json:"title"`
	Purpose         string            `json:"purpose"`
	CreatedAt       time.Time         `json:"created_at"`
	Run             RunView           `json:"run"`
	Capabilities    AgentCapabilities `json:"capabilities"`
	Approval        *ApprovalView     `json:"approval,omitempty"`
	DeliveryPending bool              `json:"delivery_pending,omitempty"`
}

type RunView struct {
	ID         RunID         `json:"id"`
	Status     string        `json:"status"`
	WaitPolicy string        `json:"wait_policy"`
	StartedAt  time.Time     `json:"started_at,omitempty"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	Output     string        `json:"output,omitempty"`
	Error      string        `json:"error,omitempty"`
	Usage      UsageSnapshot `json:"usage"`
	ToolUses   int           `json:"tool_uses,omitempty"`
}

func (r RunView) Active() bool {
	switch r.Status {
	case "queued", "starting", "running", "waiting_approval", "settling":
		return true
	}
	return false
}

// Item identity is session-local and stable across deltas and final commit.
type TranscriptItem struct {
	ID         string          `json:"id"`
	PreviousID string          `json:"previous_id,omitempty"`
	Kind       string          `json:"kind"`
	TurnID     TurnID          `json:"turn_id,omitempty"`
	StepID     StepID          `json:"step_id,omitempty"`
	CallID     CallID          `json:"call_id,omitempty"`
	Text       string          `json:"text,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Status     string          `json:"status,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
}

type TranscriptPage struct {
	Items  []TranscriptItem `json:"items"`
	Before uint64           `json:"before,omitempty"`
	More   bool             `json:"more"`
}

// Offsets are UTF-8 byte offsets. Output is addressed by a transcript item ID,
// never a caller-supplied filesystem path or arbitrary blob hash.
type OutputPage struct {
	Text  string `json:"text"`
	Next  int64  `json:"next"`
	Total int64  `json:"total"`
	More  bool   `json:"more"`
}

type AgentControl struct {
	Input      *SubmitInput  `json:"input,omitempty"`
	ID         CommandID     `json:"id"`
	SessionID  SessionID     `json:"session_id"`
	RunID      RunID         `json:"run_id,omitempty"`
	Action     string        `json:"action"`
	Text       string        `json:"text,omitempty"`
	Strategy   InputStrategy `json:"strategy,omitempty"`
	ApprovalID string        `json:"approval_id,omitempty"`
	Approve    bool          `json:"approve,omitempty"`
}

const UpdateChild UpdateType = "child_update"

// The root stream multiplexes typed child events without mixing their text
// into the parent's conversation. Full snapshots are read from the directory.
type ChildUpdate struct {
	Session ChildSession `json:"session"`
	Update  *Update      `json:"update,omitempty"`
}

// Directory readers never acquire ownership of an Agent. Open returns a
// command-checked facade; closing its subscription only detaches the observer.
type SessionDirectory interface {
	ListChildren(context.Context) ([]ChildSession, error)
	OpenReader(context.Context, SessionID) (SessionClient, error)
	ReadTranscript(context.Context, SessionID, uint64, int) (TranscriptPage, error)
	ReadOutput(context.Context, SessionID, string, int64, int) (OutputPage, error)
	Control(context.Context, AgentControl) (RunView, error)
}

// InputMessageID is the stable transcript identity of an admitted input.
func InputMessageID(id InputID) string {
	data, _ := json.Marshal(string(id))
	sum := sha256.Sum256(data)
	return "input-message-" + hex.EncodeToString(sum[:12])
}
