// Package agent implements the core agent loop. It adopts the strengths of
// both Codex and Claude Code:
//
//   - Codex: an event-driven, decoupled loop (events out, controls in) with a
//     per-tool approval gate and structured tool orchestration.
//   - Claude Code: permission modes, context compaction pipeline with tool
//     result budgets, and an interactive TUI contract.
package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/sandbox"
)

// EventType enumerates what the agent can tell the UI.
type EventType int

const (
	EventStatus          EventType = iota // transient status line (thinking, compacting…)
	EventUserMsg                          // echo of the user's message
	EventStream                           // incremental assistant text
	EventToolStart                        // a tool began executing
	EventToolResult                       // a tool finished
	EventToolStream                       // incremental output from a running tool
	EventReasoning                        // chain-of-thought deltas from the model
	EventApproval                         // approval is requested (modal)
	EventTurnDone                         // a turn completed
	EventError                            // a non-fatal error
	EventModeChanged                      // permission mode changed
	EventHistoryCleared                   // /clear happened
	EventCompacted                        // context was compacted
	EventUsage                            // token/cost accounting updated
	EventHistoryChanged                   // history edited via /remove or /rewind
	EventSandboxChanged                   // sandbox mode changed
	EventPlan                             // a plan is ready for approval (Plan Mode)
	EventPlanModeChanged                  // plan mode was toggled
)

// Usage is the accumulated token and cost accounting for the session.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int // prompt tokens served from provider cache
	Cost         float64
	TurnCount    int
}

// ToolEvent describes one tool invocation for the UI.
type ToolEvent struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Status string         `json:"status"` // running | success | error | denied
	Output string         `json:"output"` // truncated result preview
}

// ApprovalRequest is issued when a tool needs the user to decide.
type ApprovalRequest struct {
	ID      string
	Tool    string
	Command string
	Reason  string
}

// PlanRequest is issued in plan mode when the model has produced an execution
// plan that needs the user's go-ahead before any mutating work happens.
type PlanRequest struct {
	ID   string
	Plan string
}

// Event is everything the agent emits to the UI.
type Event struct {
	Type     EventType
	Text     string
	Tool     *ToolEvent
	Approval *ApprovalRequest
	Plan     *PlanRequest
	PlanMode bool
	Mode     permissions.Mode
	Usage    *Usage
}

// ControlType enumerates what the UI can tell the agent.
type ControlType int

const (
	ControlUserMessage ControlType = iota
	ControlApproval
	ControlInterrupt
	ControlClearHistory
	ControlCompactNow
	ControlSetMode
	ControlStop       // abort the current turn entirely
	ControlRemove     // remove the last N messages from history
	ControlRewind     // keep the first N messages, drop the rest
	ControlSetSandbox // switch the sandbox mode
	ControlSetPlan    // toggle plan mode on/off
	ControlPlanResp   // answer a plan approval request
)

// Control is an instruction from the UI to the agent.
type Control struct {
	Type        ControlType
	Text        string // user message text for ControlUserMessage
	ApprovalID  string // which pending approval this responds to
	Approve     bool   // true = allow, false = deny
	Remember    bool   // remember the decision for this session
	Mode        permissions.Mode
	SandboxMode sandbox.Mode
	Count       int    // number of messages for ControlRemove / ControlRewind
	PlanID      string // pending plan request this answers
	PlanApprove bool   // true = approve the plan and execute, false = reject
	PlanOn      bool   // desired plan-mode state for ControlSetPlan
}

// controlForApproval builds a Control that answers a pending approval.
func controlForApproval(id string, approve, remember bool) Control {
	return Control{Type: ControlApproval, ApprovalID: id, Approve: approve, Remember: remember}
}
