package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ccdp/internal/protocol"
)

// ActivityPhase is the small vocabulary used by the activity area.  It is
// deliberately separate from a notice: an activity describes what the
// runtime is doing now and may stay visible for a long time, while a notice
// is a short acknowledgement or warning.
type ActivityPhase string

const (
	ActivityIdle            ActivityPhase = "idle"
	ActivitySubmitting      ActivityPhase = "submitting"
	ActivityQueued          ActivityPhase = "queued"
	ActivityPreparing       ActivityPhase = "preparing"
	ActivityWaitingResponse ActivityPhase = "waiting_response"
	ActivityStreaming       ActivityPhase = "streaming"
	ActivityRunningTool     ActivityPhase = "running_tool"
	ActivityWaitingApproval ActivityPhase = "waiting_approval"
	ActivityWaitingQuestion ActivityPhase = "waiting_question"
	ActivityRetrying        ActivityPhase = "retrying"
	ActivityCompacting      ActivityPhase = "compacting"
	ActivityRestoring       ActivityPhase = "restoring"
	ActivityStopping        ActivityPhase = "stopping"
	ActivityClosed          ActivityPhase = "closed"
	ActivityCompleted       ActivityPhase = "completed"
	ActivityFailed          ActivityPhase = "failed"
	ActivityCancelled       ActivityPhase = "cancelled"
)

// Activity is the renderer-facing projection of the current runtime phase.
// IDs are retained so an old event or receipt can be rejected when a newer
// operation has already taken ownership of the activity area.
type Activity struct {
	ID          string
	SessionID   protocol.SessionID
	CommandID   protocol.CommandID
	OperationID protocol.OperationID
	TurnID      protocol.TurnID
	StepID      protocol.StepID
	ToolID      protocol.CallID
	ToolName    string
	Phase       ActivityPhase
	Label       string
	Detail      string
	StartedAt   time.Time
	UpdatedAt   time.Time
	Current     int
	Total       int
}

func (a Activity) Active() bool {
	switch a.Phase {
	case ActivityIdle, ActivityCompleted, ActivityFailed, ActivityCancelled, ActivityClosed:
		return false
	default:
		return a.Phase != ""
	}
}

// activity is intentionally value state on Model. Bubble Tea copies Model on
// every message, and keeping this projection value-owned avoids a mutable
// pointer that can outlive a session or a reader navigation generation.
func (m *Model) activityForSnapshot(snapshot protocol.SessionView) Activity {
	a := Activity{SessionID: snapshot.SessionID, TurnID: snapshotLastTurnID(snapshot), UpdatedAt: now()}
	// SessionView does not currently expose a turn ID directly. The local helper
	// returns the empty value for protocol versions that have no terminal turn.
	a.Phase = activityPhase(snapshot.Phase)
	a.Label, a.Detail = activityCopy(snapshot.Phase, snapshot.Busy, "")
	if len(snapshot.PendingInputs) > 0 && (a.Phase == ActivityIdle || a.Phase == "") {
		a.Phase = ActivityQueued
		a.Label = "等待队列"
		a.Detail = fmt.Sprintf("%d 条消息待处理", len(snapshot.PendingInputs))
	}
	if snapshot.Phase == protocol.PhaseClosed {
		a.Phase = ActivityClosed
		a.Label = "会话已关闭"
	} else if snapshot.Closing {
		a.Phase = ActivityStopping
		a.Label = "正在停止"
	}
	if a.Phase != ActivityClosed && (snapshot.Approval != nil || snapshot.Plan != nil) {
		a.Phase = ActivityWaitingApproval
		a.Label = "等待你的决定"
	}
	if a.Phase != ActivityClosed && snapshot.Question != nil {
		a.Phase = ActivityWaitingQuestion
		a.Label = "等待你的回答"
	}
	if a.Phase != ActivityClosed && snapshot.LastTurn != nil && snapshot.Phase == protocol.PhaseIdle && !snapshot.Busy && snapshot.Approval == nil && snapshot.Question == nil {
		switch snapshot.LastTurn.Status {
		case protocol.TurnFailed:
			a.Phase, a.Label, a.Detail = ActivityFailed, "本轮失败", snapshot.LastTurn.Error
		case protocol.TurnCancelled:
			a.Phase, a.Label, a.Detail = ActivityCancelled, "本轮已停止", snapshot.LastTurn.Error
		case protocol.TurnSucceeded:
			a.Phase, a.Label = ActivityCompleted, "本轮完成"
		}
	}
	return a
}

// protocol.SessionView intentionally keeps turn IDs nested in the typed
// transcript. This helper lets the activity projection remain source
// compatible while extracting a useful current turn when one is available.
func snapshotLastTurnID(s protocol.SessionView) protocol.TurnID {
	for i := len(s.Transcript) - 1; i >= 0; i-- {
		if s.Transcript[i].TurnID != "" {
			return s.Transcript[i].TurnID
		}
	}
	return ""
}

func activityPhase(phase protocol.RuntimePhase) ActivityPhase {
	switch phase {
	case protocol.PhaseIdle:
		return ActivityIdle
	case protocol.PhasePreparing:
		return ActivityPreparing
	case protocol.PhaseStreaming:
		return ActivityStreaming
	case protocol.PhaseExecutingTools:
		return ActivityRunningTool
	case protocol.PhaseWaitingApproval:
		return ActivityWaitingApproval
	case protocol.PhaseCompacting:
		return ActivityCompacting
	case protocol.PhaseStopping:
		return ActivityStopping
	case protocol.PhaseClosed:
		return ActivityClosed
	default:
		return ""
	}
}

func activityCopy(phase protocol.RuntimePhase, busy bool, detail string) (string, string) {
	if !busy && phase == protocol.PhaseIdle {
		return "", detail
	}
	switch phase {
	case protocol.PhasePreparing:
		return "准备请求", detail
	case protocol.PhaseStreaming:
		return "正在输出", detail
	case protocol.PhaseExecutingTools:
		return "正在运行工具", detail
	case protocol.PhaseWaitingApproval:
		return "等待你的决定", detail
	case protocol.PhaseCompacting:
		return "正在整理上下文", detail
	case protocol.PhaseStopping:
		return "正在停止", detail
	case protocol.PhaseClosed:
		return "会话已关闭", detail
	default:
		// A busy snapshot without a phase is not evidence of thinking. Keep the
		// activity area empty and let the snapshot's busy bit drive admission.
		return "", detail
	}
}

func (m *Model) setActivity(next Activity) {
	if next.SessionID != "" && m.sessionID != "" && next.SessionID.String() != m.sessionID {
		return
	}
	if next.UpdatedAt.IsZero() {
		next.UpdatedAt = now()
	}
	if next.StartedAt.IsZero() {
		next.StartedAt = next.UpdatedAt
	}
	m.activity = next
}

func (m *Model) beginActivity(commandID protocol.CommandID, operationID protocol.OperationID, phase ActivityPhase, label, detail string) {
	when := now()
	m.activity = Activity{ID: "operation:" + commandID.String(), SessionID: protocol.SessionID(m.sessionID), CommandID: commandID,
		OperationID: operationID, Phase: phase, Label: label, Detail: detail, StartedAt: when, UpdatedAt: when}
}

func (m *Model) updateActivityFromEvent(ev protocol.EventView) {
	if ev.SessionID != "" && ev.SessionID.String() != m.sessionID {
		return
	}
	a := Activity{SessionID: ev.SessionID, TurnID: ev.TurnID, StepID: ev.StepID, ToolID: ev.CallID, UpdatedAt: now()}
	if a.SessionID == "" {
		a.SessionID = protocol.SessionID(m.sessionID)
	}
	switch ev.Kind {
	case protocol.EventUserMessage:
		a.Phase, a.Label = ActivityPreparing, "准备请求"
		a.Detail = "消息已接收"
	case protocol.EventStream:
		a.Phase, a.Label = ActivityStreaming, "正在输出"
	case protocol.EventReasoning:
		a.Phase, a.Label = ActivityStreaming, "正在输出"
		a.Detail = "推理内容正在到达"
	case protocol.EventToolStarted, protocol.EventToolProgress:
		a.Phase, a.Label = ActivityRunningTool, "正在运行工具"
		if ev.Tool != nil {
			a.ToolID, a.ToolName, a.Detail = ev.Tool.ID, ev.Tool.Name, ev.Tool.Status
		}
	case protocol.EventApprovalRequest, protocol.EventPlanReady:
		a.Phase, a.Label = ActivityWaitingApproval, "等待你的决定"
	case protocol.EventQuestionRequest:
		a.Phase, a.Label = ActivityWaitingQuestion, "等待你的回答"
	case protocol.EventTurnDone:
		a.Phase, a.Label = ActivityCompleted, ""
	case protocol.EventError:
		a.Phase, a.Label = ActivityFailed, ""
	default:
		if ev.Phase != "" {
			a.Phase = activityPhase(ev.Phase)
			a.Label, a.Detail = activityCopy(ev.Phase, true, ev.Text)
		} else {
			return
		}
	}
	// Do not let a late terminal event clear a newer operation's activity. A
	// phase event tied to the current turn/tool is still allowed to update it.
	if current := m.activity; current.Active() && current.CommandID != "" && a.CommandID == "" &&
		current.TurnID != "" && a.TurnID != "" && current.TurnID != a.TurnID {
		return
	}
	a.ID = "event:" + string(ev.Kind) + ":" + ev.TurnID.String() + ":" + ev.CallID.String()
	a.StartedAt = m.activity.StartedAt
	m.setActivity(a)
}

func (m *Model) finishActivity(commandID protocol.CommandID, phase ActivityPhase, detail string) {
	if commandID == "" {
		if current, ok := m.operation(m.latestOperation); ok && current.Active() {
			return
		}
	}
	if commandID != "" && m.activity.CommandID != "" && m.activity.CommandID != commandID {
		return
	}
	// A runtime event with no command owner is newer and more authoritative
	// than a late UI command receipt. In particular, an approval receipt must
	// not replace a tool-running activity that has already followed it.
	if commandID != "" && m.activity.CommandID == "" && m.activity.Active() {
		return
	}
	when := now()
	m.activity.CommandID = commandID
	m.activity.Phase = phase
	m.activity.Detail = detail
	if phase == ActivityCompleted {
		m.activity.Label = ""
	}
	m.activity.UpdatedAt = when
	if m.activity.StartedAt.IsZero() {
		m.activity.StartedAt = when
	}
	if phase == ActivityCompleted || phase == ActivityFailed || phase == ActivityCancelled {
		// Keep terminal state for the presentation pass to render once. The next
		// snapshot or operation will replace it, and notices carry the durable
		// explanation for failures.
		return
	}
}

func (m Model) activeOperations() []Operation {
	var out []Operation
	for _, op := range m.operations {
		if op.Active() {
			out = append(out, op)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].CommandID < out[j].CommandID
	})
	return out
}

func (m Model) activityText() string {
	if m.activity.Label != "" {
		if m.activity.Detail != "" {
			return fmt.Sprintf("%s · %s", m.activity.Label, strings.TrimSpace(m.activity.Detail))
		}
		return m.activity.Label
	}
	return ""
}

func clonePendingInputs(src []protocol.InputView) []protocol.InputView {
	if len(src) == 0 {
		return nil
	}
	return append([]protocol.InputView(nil), src...)
}

func snapshotRevisionOlder(candidate, current protocol.Revision) bool {
	if candidate.LogSeq != 0 && current.LogSeq != 0 && candidate.LogSeq < current.LogSeq {
		return true
	}
	if candidate.LogSeq == current.LogSeq && candidate.SettingsRev != 0 && current.SettingsRev != 0 && candidate.SettingsRev < current.SettingsRev {
		return true
	}
	if candidate.LogSeq == current.LogSeq && candidate.ContextRev != 0 && current.ContextRev != 0 && candidate.ContextRev < current.ContextRev {
		return true
	}
	return false
}
