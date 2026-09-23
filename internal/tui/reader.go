package tui

import (
	"context"
	"fmt"
	"strings"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type readerKind string

const (
	readerTranscript readerKind = "transcript"
	readerOutput     readerKind = "output"
)
const readerPageSize = 16 << 10

type readerEntry struct {
	ID, Kind, Label, Text, RawText, ToolName, ToolArgs string
	CallID                                             protocol.CallID
	Truncated, OutputMore                              bool
}
type readerMatch struct{ entry, line int }

// A reader owns a bounded output page, never the terminal's native history.
// Original source is separate from its sanitized, width-dependent projection.
type readerState struct {
	workspace                   string
	kind                        readerKind
	title                       string
	sessionID                   protocol.SessionID
	itemID                      string
	entries                     []readerEntry
	index                       int
	text, rawText               string
	lines                       []string
	offset                      int
	pageOffset, next, total     int64
	more, loading               bool
	errText                     string
	lineViewport                int
	search                      string
	searchActive                bool
	matches                     []int
	locations                   []readerMatch
	matchIndex                  int
	rawMode                     bool
	request                     uint64
	returnOffset                int
	returnFollow                bool
	enterPending, screenEntered bool
	previousOffsets             []int64
	returnReader                *readerState
	historyBefore               uint64
	historyMore                 bool
	width                       int
	cacheSource                 string
	cacheIndex                  int
	cacheRaw                    bool
	cacheWidth                  int
}

func (r *readerState) currentEntry() readerEntry {
	if r == nil || r.index < 0 || r.index >= len(r.entries) {
		return readerEntry{}
	}
	return r.entries[r.index]
}
func (r *readerState) sourceText() string {
	if r == nil {
		return ""
	}
	if r.kind == readerTranscript {
		return r.currentEntry().RawText
	}
	return r.rawText
}
func (r *readerState) displayText() string {
	if r == nil {
		return ""
	}
	if r.rawMode {
		return r.sourceText()
	}
	if r.kind == readerTranscript {
		return r.currentEntry().Text
	}
	return r.text
}
func (r *readerState) entryLines(entry readerEntry, width int) []string {
	text := entry.Text
	if r.rawMode {
		text = entry.RawText
	}
	text = sanitizeANSI(text)
	if !r.rawMode && entry.Kind == "assistant" {
		text = renderMarkdown(text, width, r.workspace)
	} else if !r.rawMode && entry.Kind == "thinking" {
		header := styleBrand.Render("Thought Process:")
		lines := strings.Split(text, "\n")
		var rows []string
		rows = append(rows, header)
		for _, line := range lines {
			rows = append(rows, styleStatus.Render("│ ")+styleHints.Render(line))
		}
		text = strings.Join(rows, "\n")
	} else if !r.rawMode && isUnifiedDiff(sanitizeANSI(entry.RawText)) {
		text = sanitizeANSI(entry.ToolName+"\n"+entry.ToolArgs) + "\n\n" + renderDiff(sanitizeANSI(entry.RawText), width)
	}
	return presentationRows(text, width)
}
func (r *readerState) rebuildLines(width int) {
	if r == nil {
		return
	}
	width = max(1, width)
	r.width = width
	source := r.displayText()
	if r.cacheWidth == width && r.cacheRaw == r.rawMode && r.cacheIndex == r.index && r.cacheSource == source {
		return
	}
	if r.kind == readerTranscript {
		r.lines = r.entryLines(r.currentEntry(), width)
	} else {
		text := sanitizeANSI(source)
		if !r.rawMode && isUnifiedDiff(text) {
			text = renderDiff(text, width)
		}
		r.lines = presentationRows(text, width)
	}
	r.cacheWidth, r.cacheRaw, r.cacheIndex, r.cacheSource = width, r.rawMode, r.index, source
	r.offset = min(max(0, r.offset), r.maxOffset())
}
func (r *readerState) maxOffset() int { return max(0, len(r.lines)-max(1, r.lineViewport)) }
func (r *readerState) moveEntry(delta int) {
	if r == nil || len(r.entries) == 0 {
		return
	}
	r.index = min(max(0, r.index+delta), len(r.entries)-1)
	r.offset = 0
	r.rebuildLines(max(1, r.width))
}

func (r *readerState) findMatches() []int {
	if r == nil || r.search == "" {
		return nil
	}
	r.locations = nil
	needle := strings.ToLower(r.search)
	scan := func(lines []string, entry int) {
		for line, text := range lines {
			if strings.Contains(strings.ToLower(sanitizeANSI(text)), needle) {
				r.locations = append(r.locations, readerMatch{entry: entry, line: line})
			}
		}
	}
	if r.kind == readerTranscript {
		for i, entry := range r.entries {
			scan(r.entryLines(entry, max(1, r.width)), i)
		}
	} else {
		r.rebuildLines(max(1, r.width))
		scan(r.lines, 0)
	}
	matches := make([]int, len(r.locations))
	for i, p := range r.locations {
		matches[i] = p.line
	}
	return matches
}
func (r *readerState) selectMatch(index int) {
	if len(r.locations) == 0 {
		r.matchIndex = 0
		return
	}
	r.matchIndex = (index + len(r.locations)) % len(r.locations)
	p := r.locations[r.matchIndex]
	if r.kind == readerTranscript {
		r.index = p.entry
	}
	r.rebuildLines(max(1, r.width))
	r.offset = min(p.line, r.maxOffset())
}
func (r *readerState) nextMatch() {
	if r == nil || r.search == "" {
		return
	}
	r.matches = r.findMatches()
	r.selectMatch(r.matchIndex + 1)
}
func (r *readerState) header() string {
	if r == nil {
		return ""
	}
	position := ""
	if r.kind == readerTranscript {
		position = fmt.Sprintf(" · %d/%d", r.index+1, len(r.entries))
		if name := r.currentEntry().ToolName; name != "" {
			position += " · " + name
		}
	}
	if r.kind == readerOutput {
		position = fmt.Sprintf(" · page %d", len(r.previousOffsets)+1)
		if r.more {
			position += " · more"
		}
	}
	mode := "formatted"
	if r.rawMode {
		mode = "raw"
	}
	return r.title + position + " · " + mode
}
func (r *readerState) hint() string {
	if r == nil {
		return ""
	}
	if r.searchActive {
		return "/" + r.search + " · enter search · esc cancel"
	}
	if r.loading {
		return "loading… · esc return"
	}
	if r.kind == readerOutput {
		return "PgUp/PgDn page · / search page · c copy page · o raw · esc back"
	}
	return "↑↓ scroll · ←→ entries · / search · n next · o raw · c copy · esc back"
}

// Both the native transcript and the reader derive from the same typed
// source. Prefer live cells over an older snapshot during an active stream.
func transcriptReaderEntries(m *Model) []readerEntry {
	if m == nil {
		return nil
	}
	items := m.confirmedItems
	if len(items) == 0 && len(m.snapshot.Transcript) > 0 {
		return readerEntriesFromTranscript(m.snapshot.Transcript)
	}
	if len(items) == 0 {
		items = m.items
	}
	entries := make([]readerEntry, 0, len(items))
	for _, item := range items {
		if item.kind != "welcome" {
			entries = append(entries, item.readerEntry())
		}
	}
	return entries
}
func (item historyCell) readerEntry() readerEntry {
	text := item.text
	if item.kind == "tool" {
		text = item.toolName + "\n" + item.toolArgsRaw + "\n\n" + item.text
	}
	return readerEntry{ID: item.messageID, Kind: item.kind, Label: item.toolName, Text: text,
		RawText: item.text, ToolName: item.toolName, ToolArgs: item.toolArgsRaw,
		CallID: protocol.CallID(item.toolID), Truncated: item.toolTruncated, OutputMore: item.toolTruncated}
}
func readerEntriesFromTranscript(items []protocol.TranscriptItem) []readerEntry {
	entries := make([]readerEntry, 0, len(items))
	for _, item := range items {
		entries = append(entries, projectTranscriptCell(item).readerEntry())
	}
	return entries
}
func (m *Model) childDetailScreen() bool { return m.routing != nil && m.sessionID != m.routing.rootID }

// Join already-admitted native prints before entering the alternate screen.
func (m *Model) enterReaderScreen() tea.Cmd {
	if m.reader == nil {
		return nil
	}
	r := m.reader
	if r.screenEntered {
		return nil
	}
	if m.childDetailScreen() {
		r.enterPending = false
		return nil
	}
	if m.routing != nil && (m.routing.printing || m.routing.printScheduled || len(m.routing.printQueue) > 0) {
		r.enterPending = true
		return nil
	}
	r.enterPending = false
	r.screenEntered = true
	return tea.EnterAltScreen
}
func (m *Model) openTranscriptReader() tea.Cmd {
	entries := transcriptReaderEntries(m)
	if len(entries) == 0 {
		m.pushStatus("no transcript to read")
		return nil
	}
	m.readerSeq++
	m.readerRequest = m.readerSeq
	m.reader = &readerState{workspace: m.workspace, kind: readerTranscript, title: "Transcript details", sessionID: protocol.SessionID(m.sessionID), entries: entries, index: len(entries) - 1, request: m.readerRequest, returnOffset: m.viewport.YOffset, returnFollow: m.followOutput, historyMore: m.snapshot.TranscriptMore}
	return m.enterReaderScreen()
}
func (m *Model) openOutputReader(msg agentOutputMsg) tea.Cmd {
	if msg.sessionID != m.sessionID || msg.request != 0 && msg.request != m.readerRequest {
		return nil
	}
	previous := m.reader
	r := &readerState{workspace: m.workspace, kind: readerOutput, title: "Saved output", sessionID: protocol.SessionID(msg.sessionID), itemID: msg.itemID, text: msg.page.Text, rawText: msg.page.Text, pageOffset: msg.offset, next: msg.page.Next, total: msg.page.Total, more: msg.page.More, request: msg.request, returnOffset: m.viewport.YOffset, returnFollow: m.followOutput}
	if previous != nil {
		r.returnReader = previous
		r.screenEntered = previous.screenEntered
		r.returnOffset = previous.returnOffset
		r.returnFollow = previous.returnFollow
	}
	m.reader = r
	return m.enterReaderScreen()
}
func (m *Model) closeReader() tea.Cmd {
	if m.reader == nil {
		return nil
	}
	r := m.reader
	m.readerSeq++
	m.readerRequest = m.readerSeq
	if r.returnReader != nil {
		m.reader = r.returnReader
		m.reader.request = m.readerRequest
		m.reader.loading = false
		return nil
	}
	m.reader = nil
	m.followOutput = r.returnFollow
	m.layout()
	if !m.followOutput {
		m.viewport.SetYOffset(r.returnOffset)
	}
	if r.screenEntered && !m.childDetailScreen() {
		return tea.Sequence(tea.ExitAltScreen, m.flushInline())
	}
	return m.flushInline()
}
func (m *Model) copyReaderSource() tea.Cmd {
	if m.reader == nil {
		return nil
	}
	text := m.reader.sourceText()
	if text == "" {
		m.pushStatus("nothing to copy")
		return nil
	}
	writer := m.clipboard
	if writer == nil {
		writer = hostClipboard{}
	}
	sid, generation := m.sessionID, m.reportGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), clipboardWriteTimeout)
		defer cancel()
		err := writer.WriteAllContext(ctx, text)
		return clipboardResultMsg{sessionID: sid, generation: generation, err: err}
	}
}
func (m *Model) requestReaderPage(offset int64) tea.Cmd {
	if m.reader == nil || m.reader.loading || m.routing == nil {
		return nil
	}
	r := m.reader
	r.loading = true
	r.errText = ""
	m.readerSeq++
	r.request = m.readerSeq
	m.readerRequest = r.request
	return m.loadAgentOutputWithRequest(r.itemID, offset, r.request)
}
func (m *Model) loadNextReaderPage() tea.Cmd {
	if m.reader == nil || m.reader.kind != readerOutput || !m.reader.more || m.reader.loading || m.routing == nil {
		return nil
	}
	return m.requestReaderPage(m.reader.next)
}
func (m *Model) loadPreviousReaderPage() tea.Cmd {
	if m.reader == nil || m.reader.kind != readerOutput || m.reader.pageOffset == 0 {
		return nil
	}
	r := m.reader
	offset := max(int64(0), r.pageOffset-readerPageSize)
	if n := len(r.previousOffsets); n > 0 {
		offset = r.previousOffsets[n-1]
	}
	return m.requestReaderPage(offset)
}
func (m *Model) handleReaderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := m.reader
	if r == nil {
		return m, nil
	}
	if r.searchActive {
		switch classifyKey(msg) {
		case keyEscape:
			r.searchActive = false
			r.search = ""
			r.matches = nil
			r.locations = nil
			return m, nil
		case keyEnter:
			r.searchActive = false
			r.matches = r.findMatches()
			r.selectMatch(0)
			return m, nil
		case keyBackspace:
			chars := []rune(r.search)
			if len(chars) > 0 {
				r.search = string(chars[:len(chars)-1])
			}
			return m, nil
		}
		if msg.Type == tea.KeyRunes {
			r.search += string(msg.Runes)
		}
		return m, nil
	}
	if r.kind == readerTranscript {
		switch msg.String() {
		case "left", "shift+tab":
			if r.index == 0 && r.historyMore {
				return m, m.loadAgentHistory(r.historyBefore)
			}
			r.moveEntry(-1)
			return m, nil
		case "right", "tab":
			r.moveEntry(1)
			return m, nil
		}
	}
	switch classifyKey(msg) {
	case keyEscape, keyQuit:
		return m, m.closeReader()
	case keySearch:
		r.searchActive = true
		r.search = ""
		return m, nil
	case keyNextMatch:
		r.nextMatch()
		return m, nil
	case keyRaw:
		r.rawMode = !r.rawMode
		r.rebuildLines(max(1, r.width))
		if r.search != "" {
			r.matches = r.findMatches()
			r.selectMatch(0)
		}
		return m, nil
	case keyCopy:
		return m, m.copyReaderSource()
	case keyUp:
		r.offset = max(0, r.offset-1)
	case keyDown:
		r.offset = min(r.maxOffset(), r.offset+1)
	case keyPageUp:
		if r.offset == 0 && r.kind == readerOutput {
			return m, m.loadPreviousReaderPage()
		}
		r.offset = max(0, r.offset-readerLinePage(r))
	case keyPageDown:
		if r.offset == r.maxOffset() && r.kind == readerOutput && r.more {
			return m, m.loadNextReaderPage()
		}
		r.offset = min(r.maxOffset(), r.offset+readerLinePage(r))
	case keyHome:
		r.offset = 0
	case keyEnd:
		r.offset = r.maxOffset()
	case keyEnter:
		if r.kind == readerTranscript {
			e := r.currentEntry()
			if e.Truncated && m.routing != nil && e.ID != "" {
				return m, m.loadAgentOutput(e.ID, 0)
			}
		}
	}
	return m, nil
}

func readerLinePage(r *readerState) int {
	if r == nil {
		return 1
	}
	return max(1, r.lineViewport-1)
}
func (r *readerState) setLineViewport(height int) {
	r.lineViewport = max(1, height)
	r.offset = min(max(0, r.offset), r.maxOffset())
}
func (m *Model) readerBody(width, height int) string {
	if m.reader == nil {
		return ""
	}
	r := m.reader
	r.lineViewport = max(1, height)
	r.rebuildLines(width)
	r.setLineViewport(height)
	start := min(max(0, r.offset), r.maxOffset())
	end := min(len(r.lines), start+r.lineViewport)
	return strings.Join(r.lines[start:end], "\n")
}
func (m *Model) readerHeader() string {
	if m.reader == nil {
		return ""
	}
	return m.reader.header()
}
func (m *Model) readerHint() string {
	if m.reader == nil {
		return ""
	}
	r := m.reader
	if r.searchActive {
		query := strings.Join(strings.Fields(sanitizeANSI(r.search)), " ")
		available := max(1, m.width-2)
		if cells := lipgloss.Width(query); cells > available {
			query = ansi.TruncateLeft(query, cells-available+1, "…")
		}
		return "esc cancel · enter search\n/" + query
	}
	if r.loading {
		return "esc back · loading…"
	}
	if m.width < 32 {
		if r.kind == readerOutput {
			return "esc back · PgUp/PgDn\n/ search page · n next\no raw · c copy page"
		}
		return "esc back · ↑↓ scroll\n←→ entry · / search\nn next · o raw · c copy"
	}
	if m.width < 90 {
		if r.kind == readerOutput {
			return "esc back · PgUp/PgDn page\n/ search page · n next · o raw · c copy page"
		}
		return "esc back · ↑↓ scroll · / search\n←→ entry · n next · o raw · c copy"
	}
	return "esc back · " + strings.TrimSuffix(r.hint(), " · esc back")
}

func (m *Model) readerStatus() string {
	if m.reader == nil {
		return ""
	}
	r := m.reader
	if r.errText != "" {
		return sanitizeANSI(r.errText)
	}
	if r.search != "" && !r.searchActive {
		if len(r.matches) == 0 {
			return "no matches for /" + sanitizeANSI(r.search)
		}
		return fmt.Sprintf("match %d/%d", r.matchIndex+1, len(r.matches))
	}
	if r.kind == readerTranscript && r.currentEntry().Truncated {
		return "Preview truncated · enter opens complete saved output"
	}
	if r.kind == readerTranscript && r.historyMore {
		return "← at first entry loads older history"
	}
	return ""
}
