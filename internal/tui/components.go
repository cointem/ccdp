package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ScrollState is the small, shared scroll model used by transcript, selectors,
// and review/approval panels.  Keeping the bounds in one place is important
// on a resized or very narrow terminal: callers can move the cursor first and
// then render without having to duplicate clamping rules.
type ScrollState struct {
	Offset   int
	Viewport int
	Content  int
}

func (s *ScrollState) Set(viewport, content int) {
	if s == nil {
		return
	}
	s.Viewport = max(1, viewport)
	s.Content = max(0, content)
	s.Offset = min(max(0, s.Offset), s.MaxOffset())
}

func (s ScrollState) MaxOffset() int {
	return max(0, s.Content-s.Viewport)
}

func (s *ScrollState) Move(delta int) {
	if s == nil {
		return
	}
	s.Offset = min(max(0, s.Offset+delta), s.MaxOffset())
}

func (s *ScrollState) PageUp() {
	if s == nil {
		return
	}
	s.Move(-max(1, s.Viewport))
}

func (s *ScrollState) PageDown() {
	if s == nil {
		return
	}
	s.Move(max(1, s.Viewport))
}

func (s *ScrollState) Home() {
	if s != nil {
		s.Offset = 0
	}
}

func (s *ScrollState) End() {
	if s != nil {
		s.Offset = s.MaxOffset()
	}
}

func (s *ScrollState) EnsureVisible(index int) {
	if s == nil {
		return
	}
	if index < s.Offset {
		s.Offset = index
	}
	if index >= s.Offset+s.Viewport {
		s.Offset = index - s.Viewport + 1
	}
	s.Offset = min(max(0, s.Offset), s.MaxOffset())
}

// Focus identifies the one component allowed to consume a key.  Modal
// components are checked before the composer; this prevents an approval or a
// selector key from leaking into the input buffer.
type Focus int

const (
	FocusComposer Focus = iota
	FocusTranscript
	FocusSelector
	FocusApproval
	FocusQuestion
)

// FocusRouter is deliberately stateless.  Bubble Tea copies the root model on
// every update, so routing derived from the current model avoids a second
// mutable focus owner that could get out of sync with a closed modal.
type FocusRouter struct{}

func (FocusRouter) Target(m *Model) Focus {
	if m == nil {
		return FocusComposer
	}
	if m.question != nil {
		return FocusQuestion
	}
	if m.picker != nil && m.picker.action.Kind == selectorAgent {
		return FocusSelector
	}
	if m.approval != nil {
		return FocusApproval
	}
	if m.picker != nil {
		return FocusSelector
	}
	if !m.followOutput {
		return FocusTranscript
	}
	return FocusComposer
}

// Route handles only modal-owned keys.  Returning handled=false leaves
// transcript/composer navigation to the root key handler, preserving the
// existing textarea and history behavior.
func (r FocusRouter) Route(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m == nil {
		return m, nil, false
	}
	switch r.Target(m) {
	case FocusQuestion:
		model, cmd := m.handleQuestionKey(msg)
		return model, cmd, true
	case FocusApproval:
		model, cmd := m.handleApprovalKey(msg)
		return model, cmd, true
	case FocusSelector:
		model, cmd := m.handlePickerKey(msg)
		return model, cmd, true
	default:
		return m, nil, false
	}
}

// Selector is the reusable identity/scroll state for a menu.  pickerState
// keeps a compatibility projection for the older in-package tests and for
// callers that only need rendered labels; all new navigation goes through
// this component's stable option IDs and ScrollState.
type SelectorOption struct {
	ID          string
	Label       string
	Description string
	Current     bool
	Pending     bool
	Disabled    bool
}

type Selector struct {
	Title   string
	Options []SelectorOption
	Index   int
	Scroll  ScrollState
}

func (s *Selector) SetSize(viewport int) {
	if s == nil {
		return
	}
	s.Scroll.Set(viewport, len(s.Options))
	s.Scroll.EnsureVisible(s.Index)
}

func (s *Selector) Move(delta int) {
	if s == nil || len(s.Options) == 0 {
		return
	}
	s.Index = min(max(0, s.Index+delta), len(s.Options)-1)
	s.Scroll.EnsureVisible(s.Index)
}

func (s Selector) Selected() (SelectorOption, bool) {
	if s.Index < 0 || s.Index >= len(s.Options) {
		return SelectorOption{}, false
	}
	return s.Options[s.Index], true
}

// inlineTranscript contains only presentation state.  Runtime history remains
// in Model.confirmedItems/reports; this layer remembers which completed units
// were handed to Bubble Tea's unmanaged Println channel and bounds the active
// tail shown in the managed frame.
type inlineTranscript struct {
	printed map[string]struct{}
	// baseline contains history present when the session was attached. It is
	// emitted once to native scrollback before the managed frame is allowed to
	// discard it, even when the initial history is taller than the viewport.
	baseline map[string]struct{}
	// legacyAssociations only bridges an id-less item at its explicit logical
	// position to the first authoritative snapshot carrying a stable ID. It is
	// consumed at that bridge; no global content fingerprint can swallow two
	// distinct messages with equal text.
	legacyAssociations map[string]legacyAssociation
	// offsets are byte offsets into an active item's original text. Prefixes
	// emitted during a long stream advance this cursor; the terminal never gets
	// the same prefix again after a delta or a resize.
	offsets      map[string]int
	failedMarks  map[string]struct{}
	primed       bool
	showBaseline bool
	maxLines     int
}

type legacyAssociation struct {
	kind   string
	toolID string
	status string
	text   string
}

func (t *inlineTranscript) ensure() {
	if t.printed == nil {
		t.printed = make(map[string]struct{})
	}
	if t.baseline == nil {
		t.baseline = make(map[string]struct{})
	}
	if t.legacyAssociations == nil {
		t.legacyAssociations = make(map[string]legacyAssociation)
	}
	if t.offsets == nil {
		t.offsets = make(map[string]int)
	}
	if t.failedMarks == nil {
		t.failedMarks = make(map[string]struct{})
	}
	if t.maxLines <= 0 {
		t.maxLines = 120
	}
}

func itemIdentity(item logItem, index int) string {
	if item.messageID != "" {
		return "message:" + item.messageID
	}
	if item.reportID != "" {
		return "report:" + item.reportID
	}
	// Legacy events occasionally have no durable ID. The ordinal is only a
	// local fallback identity; it deliberately keeps equal-text siblings apart.
	return fmt.Sprintf("legacy:%s:%d", item.kind, index)
}

func legacyAssociationKey(item logItem, index int) string {
	return fmt.Sprintf("%s:%d", item.kind, index)
}

func legacyAssociationFor(item logItem) legacyAssociation {
	return legacyAssociation{kind: item.kind, toolID: item.toolID, status: item.status, text: item.text}
}

func (t *inlineTranscript) legacyMatches(item logItem, index int) bool {
	association, ok := t.legacyAssociations[legacyAssociationKey(item, index)]
	if !ok {
		return false
	}
	return association == legacyAssociationFor(item)
}

func inlineLogicalKey(item logItem, index int) string {
	if item.messageID != "" {
		return "message:" + item.messageID
	}
	if item.reportID != "" {
		return "report:" + item.reportID
	}
	if item.toolID != "" {
		return "tool:" + item.toolID
	}
	if item.kind == "assistant" {
		return "assistant:active"
	}
	return fmt.Sprintf("%s:%d", item.kind, index)
}

func (t *inlineTranscript) prime(items []logItem) {
	t.ensure()
	for i, item := range items {
		if item.kind == "assistant" && strings.TrimSpace(item.text) == "" {
			continue
		}
		id := itemIdentity(item, i)
		t.baseline[id] = struct{}{}
	}
	t.primed = true
}

func (t *inlineTranscript) showInitialFrame() {
	t.ensure()
	t.showBaseline = true
}

func (t *inlineTranscript) seen(item logItem, index int) bool {
	t.ensure()
	id := itemIdentity(item, index)
	if _, ok := t.printed[id]; ok {
		return true
	}
	return t.legacyMatches(item, index)
}

func (t *inlineTranscript) mark(item logItem, index int) {
	t.ensure()
	id := itemIdentity(item, index)
	t.printed[id] = struct{}{}
	if item.messageID == "" && item.reportID == "" {
		t.legacyAssociations[legacyAssociationKey(item, index)] = legacyAssociationFor(item)
	} else {
		// Consume the provisional id-less emission only when a flush has
		// actually observed the authoritative item. View/body calls use seen
		// as a pure predicate and therefore cannot lose a future print.
		delete(t.legacyAssociations, legacyAssociationKey(item, index))
	}
}

func (t *inlineTranscript) forgetAll() {
	t.printed = make(map[string]struct{})
	t.baseline = make(map[string]struct{})
	t.legacyAssociations = make(map[string]legacyAssociation)
	t.offsets = make(map[string]int)
	t.failedMarks = make(map[string]struct{})
	t.primed = false
	t.showBaseline = true
}

func (t *inlineTranscript) isBaseline(item logItem, index int) bool {
	t.ensure()
	id := itemIdentity(item, index)
	if _, ok := t.baseline[id]; ok {
		return true
	}
	// An id-less baseline may acquire a durable message ID in the first
	// authoritative snapshot. The ordinal/kind bridge is deliberately one-shot,
	// matching seen/mark's legacy transition rules.
	return t.legacyMatches(item, index)
}

// resetLegacyAssociations starts a new turn's provisional bridge space. A
// stream without a durable ID must not bridge into a later turn merely because
// the later item happens to reuse the same ordinal and text.
func (t *inlineTranscript) resetLegacyAssociations() {
	t.ensure()
	t.legacyAssociations = make(map[string]legacyAssociation)
}

func inlineItemComplete(item logItem, turnDone bool) bool {
	switch item.kind {
	case "assistant":
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
		return item.status != "" && item.status != "running"
	default:
		return strings.TrimSpace(item.text) != ""
	}
}

const (
	inlinePrefixBytes = 2048
	inlinePrefixLines = 16
)

func streamChunkReady(text string, offset int) bool {
	if offset < 0 || offset > len(text) {
		offset = 0
	}
	delta := text[offset:]
	return len(delta) >= inlinePrefixBytes || strings.Count(delta, "\n") >= inlinePrefixLines
}

func inlineDelta(item logItem, offset int) string {
	return inlineDeltaWidth(item, offset, 80)
}

func inlineDeltaWidth(item logItem, offset, width int) string {
	if offset < 0 || offset > len(item.text) {
		offset = 0
	}
	if offset == 0 {
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
		return indentAssistant(renderMarkdown(raw, max(1, width-2)))
	}
	return styleAssistant.Render(raw)
}

// streamCommitEnd returns the largest byte offset that ends on a stable
// newline or visual line boundary. A trailing partial line remains in the
// managed frame; this avoids tea.Println inventing paragraph breaks in the
// middle of a streamed sentence while still bounding an unbroken long token.
func streamCommitEnd(text string, width int) int {
	if text == "" {
		return 0
	}
	width = max(1, width)
	last := 0
	column := 0
	for offset, r := range text {
		size := len(string(r))
		if r == '\n' {
			last = offset + size
			column = 0
			continue
		}
		cellWidth := lipgloss.Width(string(r))
		if cellWidth == 0 {
			continue
		}
		column += cellWidth
		if column >= width {
			last = offset + size
			column = 0
		}
	}
	return last
}

func inlineDeltaRange(item logItem, offset, end int) string {
	return inlineDeltaRangeWidth(item, offset, end, 80)
}

func inlineDeltaRangeWidth(item logItem, offset, end, width int) string {
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
		return indentAssistant(renderMarkdown(raw, max(1, width-2)))
	}
	return styleAssistant.Render(raw)
}

// flushInline queues each newly completed unit for Bubble Tea's ordinary
// screen.  tea.Println is intentionally the only terminal-writing path here:
// it lets the renderer move the active frame down while leaving completed
// transcript lines in native scrollback.  The Model itself remains the source
// of truth, so tests and alternate clients can ignore this command safely.
func (m *Model) flushInline() tea.Cmd {
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
	m.inline.ensure()
	if !m.inline.primed {
		m.inline.prime(m.items)
		return nil
	}
	width := m.width - 2
	if width <= 0 {
		width = m.viewport.Width - 2
	}
	if width <= 0 {
		width = 80
	}
	var completed []string
	if m.inline.showBaseline {
		// The first managed frame may show only a tail, but the complete initial
		// history must enter native scrollback before that frame drops it. Do
		// this in the same Println batch as any newly completed item to preserve
		// order when a watch update races the initial baseline message.
		for i, item := range m.items {
			if !m.inline.isBaseline(item, i) || !inlineItemComplete(item, true) {
				continue
			}
			display := m.renderLogItem(&item, width)
			if display != "" {
				wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(display)
				completed = append(completed, strings.TrimRight(wrapped, "\n"))
			}
			m.inline.mark(item, i)
			id := itemIdentity(item, i)
			delete(m.inline.baseline, id)
		}
		m.inline.showBaseline = false
	}
	// Once the baseline has entered native scrollback, only the uncommitted
	// suffix can change. Find its left edge from the nearest committed item so a
	// live update does not rescan a long, already-printed transcript. The
	// forward pass below still preserves transcript order and handles the
	// unusual case where no committed boundary exists by falling back to zero.
	start := 0
	if !m.inline.showBaseline && len(m.items) > 0 {
		start = len(m.items) - 1
		for start >= 0 {
			item := m.items[start]
			if inlineItemComplete(item, m.turnDone) && m.inline.seen(item, start) {
				break
			}
			start--
		}
		if start < 0 {
			start = 0
		}
	}
	for i := start; i < len(m.items); i++ {
		item := m.items[i]
		key := inlineLogicalKey(item, i)
		offset := m.inline.offsets[key]
		if offset < 0 || offset > len(item.text) {
			offset = 0
		}
		legacyKey := ""
		if item.kind == "assistant" && item.messageID != "" && offset == 0 {
			// Stream events may not carry a durable message ID. Carry the
			// active prefix offset into the authoritative snapshot item.
			legacyKey = "assistant:active"
			if carried := m.inline.offsets[legacyKey]; carried > 0 {
				offset = carried
			}
		}
		complete := inlineItemComplete(item, m.turnDone)
		if !complete {
			// Emit a stable prefix for a genuinely long active stream. The
			// original item stays in history, while offset keeps the managed
			// tail and the unmanaged transcript from re-rendering O(n²) text.
			if (item.kind == "assistant" || item.kind == "tool") &&
				(streamChunkReady(item.text, offset) ||
					// A typed stream can be small but already end in a complete
					// Markdown unit (for example the first short paragraph). Print
					// that unit once while retaining the active offset; waiting for
					// the byte/line threshold would leave the first response marker
					// invisible until a much later delta arrives.
					(offset == 0 && item.messageID != "")) {
				// Markdown must freeze only complete source blocks. Plain text still
				// falls back to bounded visual lines inside
				// markdownStablePrefixEnd; the original source remains untouched.
				deltaEnd := markdownStablePrefixEnd(item.text, offset, width)
				if deltaEnd <= offset {
					continue
				}
				display := inlineDeltaRangeWidth(item, offset, deltaEnd, width)
				if display != "" {
					wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(display)
					completed = append(completed, strings.TrimRight(wrapped, "\n"))
				}
				m.inline.offsets[key] = deltaEnd
			}
			continue
		}
		if m.inline.seen(item, i) {
			if item.messageID != "" || item.reportID != "" {
				m.inline.mark(item, i)
			}
			continue
		}
		display := inlineDeltaWidth(item, offset, width)
		if item.kind == "welcome" {
			display = m.renderLogItem(&item, width)
		}
		if display != "" {
			wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(display)
			completed = append(completed, strings.TrimRight(wrapped, "\n"))
		}
		if m.turnFailed && item.kind == "assistant" && item.messageID == "" {
			if _, marked := m.inline.failedMarks[key]; !marked {
				completed = append(completed, styleError.Render("✗ cancelled/failed"))
				m.inline.failedMarks[key] = struct{}{}
			}
		}
		m.inline.mark(item, i)
		delete(m.inline.offsets, key)
		if legacyKey != "" {
			delete(m.inline.offsets, legacyKey)
		}
	}
	if len(completed) == 0 {
		return nil
	}
	m.inline.showBaseline = false
	text := strings.Join(completed, "\n")
	if m.routing != nil {
		generation := m.routing.generation
		m.routing.printScheduled = true
		return func() tea.Msg { return inlinePrintMsg{generation: generation, text: text} }
	}
	return tea.Println(text)
}

func (m *Model) inlineBody() string {
	if m == nil || !m.inlineMode {
		return ""
	}
	m.inline.ensure()
	width := m.width - 2
	if width <= 0 {
		width = m.viewport.Width - 2
	}
	if width <= 0 {
		width = 80
	}
	limit := m.inline.maxLines
	if m.height > 0 {
		// The managed frame should never grow without bound while a provider
		// sends a long stream. Native scrollback receives completed units via
		// flushInline; this is only the live tail and the current prompt.
		limit = max(1, m.height-6)
		if m.viewport.Height > 0 {
			limit = m.viewport.Height
		}
	}
	// Scan from the tail. Completed units have already entered native
	// scrollback and are skipped; stopping once the managed viewport is full
	// keeps active streaming work proportional to the visible tail rather than
	// the entire transcript history.
	type inlineChunk struct {
		kind string
		text string
	}
	var chunks []inlineChunk
	usedRows := 0
	for i := len(m.items) - 1; i >= 0; i-- {
		item := m.items[i]
		if !m.inline.showBaseline && inlineItemComplete(item, m.turnDone) && m.inline.seen(item, i) {
			// Items are committed in transcript order. Once the active tail has
			// contributed rows, the first committed item below it is a stable
			// boundary; older history is already in native scrollback and need
			// not be scanned on every frame.
			break
		}
		key := inlineLogicalKey(item, i)
		offset := m.inline.offsets[key]
		if item.kind == "assistant" && item.messageID != "" && offset == 0 {
			offset = m.inline.offsets["assistant:active"]
		}
		display := ""
		if offset > 0 {
			// Once a prefix has entered scrollback, render only the suffix even
			// during the short terminal transition where the turn becomes
			// complete. This keeps a long stream from flashing/re-rendering its
			// full history before flushInline prints the final suffix.
			display = inlineDeltaWidth(item, offset, width)
		} else {
			display = m.renderLogItem(&item, width)
		}
		itemRows := boundDisplayLines(display, width, limit)
		if len(itemRows) == 0 {
			continue
		}
		// Each older item is prepended below after reversing the chunk list.
		chunks = append(chunks, inlineChunk{kind: item.kind, text: strings.Join(itemRows, "\n")})
		usedRows += len(itemRows)
		if usedRows >= limit {
			break
		}
	}
	if len(chunks) == 0 {
		return " "
	}
	lines := make([]string, 0, min(limit, usedRows))
	previousKind := ""
	for i := len(chunks) - 1; i >= 0; i-- {
		if len(lines) > 0 && transcriptGap(previousKind, chunks[i].kind) {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(chunks[i].text, "\n")...)
		previousKind = chunks[i].kind
		if len(lines) >= limit {
			lines = lines[len(lines)-limit:]
		}
	}
	if len(lines) == 0 {
		return " "
	}
	return strings.Join(lines, "\n")
}

func transcriptGap(previous, current string) bool {
	return (previous == "user" || previous == "command") && current == "assistant"
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
