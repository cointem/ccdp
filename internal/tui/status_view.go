package tui

import (
	"ccdp/internal/protocol"
	"fmt"
	"strings"
	"time"
)

func (m *Model) renderStatus() string {
	width := max(1, m.width-2)
	if m.width < 34 && (m.approval != nil || m.question != nil || m.picker != nil) {
		// Decision and selector surfaces own the actionable status on narrow
		// terminals. Dropping this duplicate row reserves space for the target,
		// review title, and persistent model/context footer.
		return ""
	}
	if m.approval != nil {
		return " " + fitLines(styleRunning.Render("◇ Your call")+styleHints.Render(" · awaiting your decision"), width)
	}
	status := ""
	if notice, ok := m.latestStatusNotice(); ok {
		status = truncateDisplay(sanitizeANSI(notice.Text), width)
	}
	activity := ""
	if m.activity.Active() {
		activity = truncateDisplay(sanitizeANSI(m.activityText()), width)
	}
	if status == "" && activity == "" {
		if operations := m.activeOperations(); len(operations) > 0 {
			status = truncateDisplay("operation pending · "+string(operations[len(operations)-1].Type), width)
		}
	}
	if queued := len(m.pendingInputs); queued > 0 {
		queueText := fmt.Sprintf("%d queued", queued)
		if activity == "" && status == "" {
			status = queueText
		} else if !strings.Contains(status, queueText) && !strings.Contains(activity, queueText) {
			if activity != "" {
				activity += " · " + queueText
			} else {
				status += " · " + queueText
			}
		}
	}
	// Activity is the runtime fact for work in progress. A notice can replace
	// it only when it is a higher-severity decision/error acknowledgement; this
	// keeps an old completion toast from claiming that a newer tool is idle.
	if activity != "" {
		status = activity
	}
	for _, notice := range m.visibleNotices() {
		if notice.Severity == NoticeDecision || notice.Severity == NoticeError {
			status = truncateDisplay(sanitizeANSI(notice.Text), width)
			break
		}
	}
	if m.busy || activity != "" {
		if status == "" {
			if m.streaming {
				status = "正在输出"
			} else {
				status = m.workVerb()
			}
		}
		elapsed := ""
		if !m.turnStarted.IsZero() {
			elapsed = " · " + formatElapsed(time.Since(m.turnStarted))
		}
		spinner := m.spinner.View()
		if !m.animateWork() {
			spinner = "·"
		}
		statusStyle := styleRunning
		if m.turnStalled() {
			elapsed += " · 等待模型响应"
		}
		return " " + fitLines(spinner+" "+statusStyle.Render(status)+styleHints.Render(elapsed), width)
	}
	if status != "" {
		return " " + fitLines(styleStatus.Render("· "+status), width)
	}
	if m.turnDone && !m.turnStarted.IsZero() {
		if m.turnFailed {
			if !m.interruptRequested {
				// Failed turns already have a durable error explanation. Do not
				// replace its duplicate toast with another permanent status row.
				return ""
			}
			return " " + fitLines(styleToolErr.Render("■ Turn stopped")+styleHints.Render(" · ready for your next message"), width)
		}
		// Completion is represented by the final answer and per-turn duration
		// in the transcript. A permanent “All set” row only pushes useful
		// transcript and input content down on short terminals.
		return ""
	}
	return ""
}

// renderComposerFeedback renders the receipt of the last submitted command
// ("model → sonnet", "effort → xhigh") for the hint row immediately above the
// composer. A slash command that only adjusts state answers the submission
// rather than the conversation, so its acknowledgement belongs to the input
// zone: next to the prompt it cannot be read as one more row of output, and
// because the row is always reserved the transcript never shifts when the hint
// appears or expires.
func (m *Model) renderComposerFeedback(budget int) string {
	if budget <= 0 {
		return ""
	}
	notice, ok := m.latestFeedback()
	if !ok {
		return ""
	}
	// A command still in flight is already reported by the status lane with its
	// spinner, so its placeholder is not repeated here.
	if op, known := m.operations[notice.CommandID]; known && op.Active() {
		return ""
	}
	text := truncateDisplay(sanitizeANSI(notice.Text), budget)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return styleStatus.Render(text)
}

// workVerb is the fallback label for a busy turn with no runtime-phase evidence.
// The choice is seeded by the turn start so it stays stable for the whole turn
// yet varies between turns; it never invents activity when there is no phase.
func (m *Model) workVerb() string {
	verbs := []string{
		"正在处理", "正在思考", "正在推进", "正在梳理", "正在推敲",
		"正在盘算", "正在琢磨", "正在酝酿", "正在张罗", "正在打点",
	}
	if m.turnStarted.IsZero() {
		return verbs[0]
	}
	idx := int(m.turnStarted.UnixNano() % int64(len(verbs)))
	if idx < 0 {
		idx += len(verbs)
	}
	return verbs[idx]
}

// A quiet stream is not evidence of a hung runtime. Only model-facing waits
// get a neutral delay hint; tools, child runs and user decisions may be quiet.
func (m *Model) turnStalled() bool {
	if !m.busy || m.watchDisconnected || m.approval != nil || m.question != nil || m.activity.UpdatedAt.IsZero() {
		return false
	}
	switch m.activity.Phase {
	case ActivityPreparing, ActivityWaitingResponse, ActivityStreaming:
	default:
		return false
	}
	for _, item := range m.items {
		if item.kind == "tool" && !toolStatusComplete(item.status) && !toolStatusIsFailure(item.status) {
			return false
		}
	}
	if m.routing != nil {
		for _, child := range m.routing.rows {
			if child.ParentSessionID == protocol.SessionID(m.sessionID) && child.Run.Active() {
				return false
			}
		}
	}
	return now().Sub(m.activity.UpdatedAt) > 30*time.Second
}

func formatElapsed(elapsed time.Duration) string {
	seconds := max(0, int(elapsed.Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}

// renderTasks draws the working task checklist toggled by ctrl+t. It is omitted
// entirely when hidden or when the session has produced no tasks, so the panel
// never reserves space by accident. Statuses mirror the todo store vocabulary.
func (m *Model) renderTasks() string {
	if !m.tasksVisible {
		return ""
	}
	tasks := m.snapshot.Tasks
	if len(tasks) == 0 {
		return ""
	}
	width := max(1, m.width-2)
	done := 0
	for _, t := range tasks {
		if t.Status == "completed" {
			done++
		}
	}
	header := styleModalTitle.Render("Tasks") + styleHints.Render(fmt.Sprintf(" · %d/%d done", done, len(tasks)))
	rows := []string{" " + fitLines(header, width)}
	for i, t := range tasks {
		if i >= maxTaskRows {
			rows = append(rows, " "+styleHints.Render(fmt.Sprintf("… +%d more", len(tasks)-maxTaskRows)))
			break
		}
		rows = append(rows, " "+renderTaskRow(t, width))
	}
	return strings.Join(rows, "\n")
}

// renderTaskRow prefixes each task with a status glyph: ✔ completed, ●
// in progress, ○ pending. Titles are truncated to one line so the checklist
// stays scannable at any width.
func renderTaskRow(t protocol.TaskView, width int) string {
	title := truncateDisplay(sanitizeANSI(t.Title), max(1, width-3))
	switch t.Status {
	case "completed":
		return styleToolOK.Render("✔ ") + styleStatus.Render(title)
	case "in_progress":
		return styleRunning.Render("● ") + styleAssistant.Render(title)
	default:
		return styleDivider.Render("○ ") + styleStatus.Render(title)
	}
}

// Animate only execution, never completed work or a decision waiting on input.
func (m *Model) animateWork() bool {
	if m.watchDisconnected || m.approval != nil || m.question != nil {
		return false
	}
	switch m.activity.Phase {
	case ActivityWaitingApproval, ActivityWaitingQuestion, ActivityClosed, ActivityCompleted, ActivityFailed, ActivityCancelled:
		return false
	}
	return m.busy || m.activity.Active()
}
