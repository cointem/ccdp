package agent

import (
	"ccdp/internal/protocol"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// traceRec is one JSONL line in the diagnostics stream. It is deliberately
// not a source for restoring business state; the typed session store is the
// only replay authority.
type traceRec struct {
	Question *protocol.QuestionRequest `json:"question,omitempty"`
	TS       string                    `json:"ts"`
	Type     string                    `json:"type"`
	Text     string                    `json:"text,omitempty"`

	Tool     *ToolEvent       `json:"tool,omitempty"`
	Approval *ApprovalRequest `json:"approval,omitempty"`
	Plan     *PlanRequest     `json:"plan,omitempty"`
	Mode     string           `json:"mode,omitempty"`
	Usage    *Usage           `json:"usage,omitempty"`
}

// eventTypeName maps an event to a stable JSONL name.
func eventTypeName(t EventType) string {
	switch t {
	case EventStatus:
		return "status"
	case EventUserMsg:
		return "user"
	case EventStream:
		return "stream"
	case EventToolStart:
		return "tool_start"
	case EventToolResult:
		return "tool_result"
	case EventToolStream:
		return "tool_stream"
	case EventApproval:
		return "approval"
	case EventTurnDone:
		return "turn_done"
	case EventError:
		return "error"
	case EventModeChanged:
		return "mode"
	case EventHistoryCleared:
		return "history_cleared"
	case EventCompacted:
		return "compacted"
	case EventUsage:
		return "usage"
	case EventHistoryChanged:
		return "history_changed"
	case EventSandboxChanged:
		return "sandbox"
	case EventQuestion:
		return "question"
	case EventPlan:
		return "plan"
	case EventPlanModeChanged:
		return "plan_mode"
	}
	return "unknown"
}

// openTrace creates the per-session diagnostics stream.  Older versions wrote
// <root>/<id>.trace.jsonl; new diagnostics live below the same session scope as
// events and never participate in List/Resume replay.
func (a *Agent) openTrace() {
	if a.cfg.SessionDir == "" || a.cfg.NoSessionPersistence {
		return
	}
	path := a.TracePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path,
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	a.trace = f
}

// writeTrace appends one event to the trace file (best-effort, never blocks).
func (a *Agent) writeTrace(ev Event) {
	rec := traceRec{
		TS:   time.Now().Format(time.RFC3339Nano),
		Type: eventTypeName(ev.Type),
		Text: ev.Text,
	}
	switch {
	case ev.Tool != nil:
		rec.Tool = ev.Tool
	case ev.Approval != nil:
		rec.Approval = ev.Approval
	case ev.Question != nil:
		rec.Question = ev.Question
	case ev.Plan != nil:
		rec.Plan = ev.Plan
	case ev.Usage != nil:
		rec.Usage = ev.Usage
	}
	if ev.Mode != "" {
		rec.Mode = string(ev.Mode)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	a.traceMu.Lock()
	defer a.traceMu.Unlock()
	if a.trace == nil {
		return
	}
	_, _ = a.trace.Write(append(b, '\n'))
}

// closeTrace flushes and closes the trace file. The nil check and the close
// both happen under traceMu so it cannot race with a running turn's
// writeTrace (Close may be called while a turn is still streaming).
func (a *Agent) closeTrace() {
	a.traceMu.Lock()
	if a.trace != nil {
		_ = a.trace.Close()
		a.trace = nil
	}
	a.traceMu.Unlock()
}

// TracePath returns the diagnostics path for this session.  It is retained as
// a compatibility accessor for hooks/tests; restore never reads this file.
func (a *Agent) TracePath() string {
	if a.cfg.SessionDir == "" || a.sessionID == "" {
		return ""
	}
	return filepath.Join(a.cfg.SessionDir, a.sessionID, "diagnostics.jsonl")
}
