package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// historyLedger contains only presentation state.  Runtime history remains
// in Model.confirmedItems/reports; this layer remembers which completed units
// were handed to Bubble Tea's unmanaged Println channel and bounds the active
// tail shown in the managed frame.
type historyLedger struct {
	inheritedPrinted map[string]struct{}
	lastKind         string
	nextIndex        int
	boundaryID       string
	epoch            uint64
	printed          map[string]struct{}
	// offsets are byte offsets into an active item's original text. Prefixes
	// emitted during a long stream advance this cursor; the terminal never gets
	// the same prefix again after a delta or a resize.
	offsets      map[string]int
	primed       bool
	showBaseline bool
	maxLines     int
}

func (t *historyLedger) ensure() {
	if t.printed == nil {
		t.printed = make(map[string]struct{})
	}
	if t.offsets == nil {
		t.offsets = make(map[string]int)
	}
	if t.maxLines <= 0 {
		t.maxLines = 120
	}
}

func itemIdentity(item historyCell, index int) string {
	if item.messageID != "" {
		return "message:" + item.messageID
	}
	if item.reportID != "" {
		return "report:" + item.reportID
	}
	return ""
}

func inlineLogicalKey(item historyCell, index int) string {
	return itemIdentity(item, index)
}

func (t *historyLedger) prime(items []historyCell) {
	t.ensure()
	t.primed = true
}

func (t *historyLedger) showInitialFrame() {
	t.ensure()
	t.showBaseline = true
}

func (t *historyLedger) seen(item historyCell, index int) bool {
	t.ensure()
	id := itemIdentity(item, index)
	if _, ok := t.printed[id]; ok {
		return true
	}
	if _, ok := t.inheritedPrinted[id]; ok {
		return true
	}
	return false
}

func (t *historyLedger) mark(item historyCell, index int) {
	t.ensure()
	id := itemIdentity(item, index)
	t.printed[id] = struct{}{}

}

// resetNativeHistory re-baselines presentation after the runtime replaced the
// conversation projection (for example /clear or a rewind). Completed rows were
// already handed to the terminal's native scrollback, and UI-local reports
// describe the same discarded conversation, so both are forgotten; the
// authoritative empty transcript is rebuilt by the resetting snapshot itself.
// nativeHistoryResetPending tells the update loop to erase that scrollback,
// which a managed-frame repaint cannot reach.
func (m *Model) resetNativeHistory() {
	m.reports = nil
	m.selActive, m.selAnchor, m.selFocus = false, nil, nil
	m.copiedToast = ""
	m.inline.forgetAll()
	m.nativeHistoryResetPending = true
}

// eraseNativeHistory erases the terminal's native scrollback together with the
// visible screen. Inline mode prints completed rows through tea.Println, so a
// conversation reset cannot be expressed by repainting the managed frame: the
// rows already written have to be removed from the terminal itself. CSI 3J is
// deliberately confined to this path — navigation never emits it.
func (m *Model) eraseNativeHistory() tea.Cmd {
	if m.terminal != nil {
		m.terminal.WriteRaw("\x1b[3J")
	}
	return tea.ClearScreen
}

func (t *historyLedger) forgetAll() {
	t.epoch++
	t.nextIndex, t.boundaryID = 0, ""
	t.inheritedPrinted = nil
	t.lastKind = ""
	t.printed = make(map[string]struct{})
	t.offsets = make(map[string]int)
	t.primed = false
	t.showBaseline = true
}

func inlineItemComplete(item historyCell, turnDone bool) bool {
	switch item.kind {
	case "assistant", "thinking":
		if strings.TrimSpace(item.text) == "" {
			return false
		}
		// Live protocol transcript rows already have a stable stream ID, but
		// that identity is not a completion signal. The provider reuses it for
		// every cumulative delta while status remains streaming; treating the ID
		// as printed here drops all text after the first fragment. Keep the row
		// active until its status reaches a terminal value (or the enclosing turn
		// explicitly completes).
		switch strings.ToLower(strings.TrimSpace(item.status)) {
		case "streaming", "running", "queued", "pending", "starting":
			return false
		}
		return turnDone || item.messageID != ""
	case "tool":
		switch strings.ToLower(strings.TrimSpace(item.status)) {
		case "success", "done", "completed", "complete", "error", "failed", "failure", "denied", "rejected", "cancelled", "canceled", "stopped", "interrupted":
			return true
		}
		return false
	default:
		return strings.TrimSpace(item.text) != ""
	}
}

func inlineDeltaWidth(item historyCell, offset, width int, workspace ...string) string {
	if offset < 0 || offset > len(item.text) {
		offset = 0
	}
	if offset == 0 {
		if item.kind == "assistant" {
			return renderAssistantCell(item.cleanText(), width, workspace...)
		}
		return renderItemWidth(&item, width)
	}
	raw := sanitizeANSI(item.text[offset:])
	if raw == "" {
		return ""
	}
	if item.kind == "tool" {
		return raw
	}
	if item.kind == "assistant" {
		return indentAssistant(renderMarkdown(raw, max(1, width-2), workspace...))
	}
	return styleAssistant.Render(raw)
}

func inlineDeltaRangeWidth(item historyCell, offset, end, width int, workspace ...string) string {
	if offset < 0 {
		offset = 0
	}
	if end > len(item.text) {
		end = len(item.text)
	}
	if offset >= end {
		return ""
	}
	if offset == 0 {
		part := item
		part.text = item.text[:end]
		if part.kind == "assistant" {
			return renderAssistantCell(part.cleanText(), width, workspace...)
		}
		return renderItemWidth(&part, width)
	}
	raw := sanitizeANSI(item.text[offset:end])
	if raw == "" {
		return ""
	}
	if item.kind == "tool" {
		return raw
	}
	if item.kind == "assistant" {
		return indentAssistant(renderMarkdown(raw, max(1, width-2), workspace...))
	}
	return styleAssistant.Render(raw)
}

// flushInline schedules a draft history transaction. Only a matching write
// acknowledgment may publish its offsets and completed cell identities.
func (m *Model) flushInline() tea.Cmd {
	if m == nil || m.delivery.pending != nil {
		return nil
	}
	// Production uses an acknowledged ledger. The planner mutates an isolated
	// candidate; committed offsets remain unchanged until the writer responds.
	if m.terminal == nil || m.routing == nil {
		return nil
	}
	before := m.inline
	m.inline = before.clone()
	cmd := m.planHistory()
	if cmd == nil {
		candidate := m.inline
		m.inline = before
		m.inline.accept(candidate)
		return nil
	}
	candidate := m.inline
	m.inline = before
	m.delivery.sequence++
	id, generation := m.delivery.sequence, m.routing.generation
	m.delivery.pending = &historyCommit{id: id, generation: generation, next: candidate}
	return func() tea.Msg {
		msg := cmd()
		if print, ok := msg.(inlinePrintMsg); ok {
			print.batch = id
			return print
		}
		return msg
	}
}

func (m *Model) planHistory() tea.Cmd {
	// A detail reader owns the managed/alternate screen. Do not mark transcript
	// rows printed while it is open: the pending root output must remain queued
	// and flush after the reader returns, otherwise the main view silently loses
	// those rows from native scrollback.
	if m != nil && m.reader != nil {
		return nil
	}
	if m != nil && m.routing != nil && (m.routing.printing || m.routing.printScheduled) {
		return nil
	}
	if m == nil || !m.inlineMode {
		return nil
	}
	if m.routing != nil && (m.routing.pending != nil || m.routing.switching) {
		return nil
	}
	width := max(1, m.width-2)
	text := m.inline.plan(m.items, width, m.turnDone, m.renderLogItem, m.workspace)
	if text == "" {
		return nil
	}
	generation := uint64(0)
	if m.routing != nil {
		generation = m.routing.generation
		m.routing.printScheduled = true
	}
	return func() tea.Msg { return inlinePrintMsg{generation: generation, text: text} }
}

// plan is a terminal-independent ordered cell projection. It mutates a draft
// ledger; the caller publishes that draft only after the output receipt.
func (t *historyLedger) plan(items []historyCell, width int, turnDone bool, render func(*historyCell, int) string, workspace string) string {
	t.ensure()
	if !t.primed {
		t.prime(items)
		return ""
	}
	var completed []string
	start := 0
	if t.nextIndex > 0 && t.nextIndex <= len(items) && itemIdentity(items[t.nextIndex-1], t.nextIndex-1) == t.boundaryID {
		start = t.nextIndex
	}
	for i := start; i < len(items); i++ {
		item := items[i]
		key := inlineLogicalKey(item, i)
		if key == "" {
			break
		}
		if t.seen(item, i) {
			t.nextIndex, t.boundaryID = i+1, key
			continue
		}
		offset := t.offsets[key]
		if offset < 0 || offset > len(item.text) {
			offset = 0
		}
		complete := inlineItemComplete(item, turnDone)
		if !complete {
			// A running delegation is shown as a compact marker in the managed
			// live tail and must never stream raw child text into native
			// scrollback. Skip incremental prefix printing and keep its offset at
			// zero so the completion path renders the whole marker once.

			// Tools must never flush while incomplete, otherwise they become frozen
			// as 'running' in the immutable terminal scrollback. Keep incomplete tools
			// in the managed live tail until complete.
			if item.kind != "assistant" {
				break
			}
			deltaEnd := (StreamController{}).CommitEnd(item.text, offset, width, item.messageID != "")
			if deltaEnd > offset {
				display := inlineDeltaRangeWidth(item, offset, deltaEnd, width, workspace)
				if display != "" {
					wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(display)
					if transcriptGap(t.lastKind, item.kind) {
						completed = append(completed, "")
					}
					completed = append(completed, strings.TrimRight(wrapped, "\n"))
					t.lastKind = item.kind
				}
				t.offsets[key] = deltaEnd
			}
			break
		}
		display := inlineDeltaWidth(item, offset, width, workspace)
		if offset == 0 {
			display = render(&item, width)
		}
		if display != "" {
			wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(display)
			if transcriptGap(t.lastKind, item.kind) {
				completed = append(completed, "")
			}
			completed = append(completed, strings.TrimRight(wrapped, "\n"))
			t.lastKind = item.kind
		}
		t.mark(item, i)
		t.nextIndex, t.boundaryID = i+1, key
		delete(t.offsets, key)

	}
	if len(completed) == 0 {
		return ""
	}
	t.showBaseline = false
	return strings.Join(completed, "\n")
}

func transcriptGap(previous, current string) bool {
	return previous != "" && previous != current && previous != "user" && current != "user"
}

func appendInlineFlush(model tea.Model, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	switch current := model.(type) {
	case *Model:
		// Keep command-producing key paths as a single command.  Bubble Tea
		// represents a batch as a BatchMsg, which means callers that execute a
		// returned command directly (and, more importantly, protocol receipts)
		// would otherwise never see the receipt.  The next protocol update will
		// flush the already committed inline state; a command-less update can
		// still print the pending transcript immediately.
		if cmd != nil {
			return current, cmd
		}
		return current, current.flushInline()
	case Model:
		if cmd != nil {
			return current, cmd
		}
		return current, (&current).flushInline()
	default:
		return model, cmd
	}
}

func boundDisplayLines(text string, width, limit int) []string {
	if limit <= 0 {
		limit = 1
	}
	if text == "" {
		return nil
	}
	wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(text)
	lines := strings.Split(strings.TrimRight(wrapped, "\n"), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
		if len(lines) > 0 {
			lines[0] = "… " + strings.TrimLeft(lines[0], " ")
		}
	}
	return lines
}

// reconcile carries acknowledged and pending prefixes across the runtime's
// explicit stream-to-durable identity transition. No text/ordinal inference.
func (t *historyLedger) reconcile(old, next historyCell) {
	t.ensure()
	oldID, newID := itemIdentity(old, 0), itemIdentity(next, 0)
	if oldID == "" || newID == "" {
		return
	}
	if old.text == next.text && t.seen(old, 0) {
		t.printed[newID] = struct{}{}
	}
	if offset, ok := t.offsets[oldID]; ok {
		offset = min(max(0, offset), len(old.text))
		if offset <= len(next.text) && strings.HasPrefix(next.text, old.text[:offset]) {
			t.offsets[newID] = offset
		}
	}
	t.nextIndex, t.boundaryID = 0, ""
}
