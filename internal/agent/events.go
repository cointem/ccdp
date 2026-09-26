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
	"ccdp/internal/protocol"
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
	EventQuestion
	EventSessionChanged // the agent switched to another session (fork/resume)
)

// Usage is the accumulated token and cost accounting for the session.
type Usage struct {
	Cache        protocol.CacheStats `json:"cache,omitempty"`
	InputTokens  int                 `json:"input_tokens"`
	OutputTokens int                 `json:"output_tokens"`
	CachedTokens int                 `json:"cached_tokens"` // prompt tokens served from provider cache
	Cost         float64             `json:"cost"`
	TurnCount    int                 `json:"turn_count"`
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
	Capabilities []protocol.CapabilityRequest
	// journalID is the occurrence-scoped durable identity. ID remains the
	// compatibility/UI call ID so older control clients can answer a prompt;
	// durable ApprovalResolved facts use journalID instead.
	journalID string
}

// PlanRequest is issued in plan mode when the model has produced an execution
// plan that needs the user's go-ahead before any mutating work happens.
type PlanRequest struct {
	ID   string
	Plan string
}

// Event is everything the agent emits to the UI.
type Event struct {
	Question  *protocol.QuestionRequest
	Type      EventType
	Text      string
	TurnID    string // explicit turn identity for terminal/error events
	MessageID string // stable history identity for EventUserMsg de-duplication
	Tool      *ToolEvent
	Approval  *ApprovalRequest
	Plan      *PlanRequest
	PlanMode  bool
	Mode      permissions.Mode
	Usage     *Usage
}
