package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

// Routing state belongs to the frontend, never to a running Agent. Stored
// views contain drafts and scroll state, but no subscription ownership.
type sessionRouting struct {
	directory      protocol.SessionDirectory
	rootID         string
	rootClient     protocol.SessionClient
	views          map[string]Model
	drafts         map[string]sessionDraft
	order          []string
	rows           []protocol.ChildSession
	request        uint64
	generation     uint64
	printing       bool
	printScheduled bool
	printQueue     []inlinePrintMsg
	pending        *agentViewOpenedMsg
	switching      bool
	catalogLoading bool
}

type agentCatalogMsg struct {
	directory protocol.SessionDirectory
	rows      []protocol.ChildSession
	err       error
	show      bool
}
type agentViewOpenedMsg struct {
	routing  *sessionRouting
	request  uint64
	client   protocol.SessionClient
	snapshot protocol.SessionView
	err      error
}
type agentViewResetMsg struct {
	generation uint64
	err        error
}
type agentControlMsg struct {
	id  protocol.SessionID
	run protocol.RunView
	err error
}
type inlinePrintMsg struct {
	batch      uint64
	generation uint64
	text       string
}
type inlinePrintedMsg struct {
	generation uint64
	batch      uint64
	err        error
}

var renderGeneration atomic.Uint64

type sessionDraft struct {
	missingImages       bool
	text, retry         string
	images, retryImages []protocol.InputImage
	retryID             protocol.CommandID
	follow              bool
	offset              int
	paste               *pasteFoldState
}

func (m *Model) installSessionRouting(directory protocol.SessionDirectory) {
	if directory == nil {
		return
	}
	m.delivery.pending = nil
	m.routing = &sessionRouting{directory: directory, rootID: m.sessionID, rootClient: m.client, views: make(map[string]Model), drafts: make(map[string]sessionDraft), generation: renderGeneration.Add(1)}
}

func (m *Model) loadAgentCatalog(show bool) tea.Cmd {
	if m.routing == nil {
		m.pushStatus("agent directory unavailable")
		return nil
	}
	if m.routing.catalogLoading && !show {
		return nil
	}
	m.routing.catalogLoading = true
	directory := m.routing.directory
	return func() tea.Msg {
		rows, err := directory.ListChildren(context.Background())
		return agentCatalogMsg{directory: directory, rows: rows, err: err, show: show}
	}
}

func (m *Model) openAgentPicker() tea.Cmd {
	if m.routing == nil {
		return nil
	}
	if m.sessionID == m.routing.rootID && len(m.routing.rows) == 1 {
		return m.openAgentView(string(m.routing.rows[0].SessionID))
	}
	return m.agentPicker()
}

func (m *Model) agentPicker() tea.Cmd {
	options := []SelectorOption{{ID: m.routing.rootID, Label: "main · " + m.routing.rootID}}
	selected := 0
	for _, row := range m.routing.rows {
		mark := ""
		if childAwaitingApproval(row) {
			mark = " · approval required"
		}
		if string(row.SessionID) == m.sessionID {
			selected = len(options)
		}
		options = append(options, SelectorOption{ID: string(row.SessionID), Label: fmt.Sprintf("%s · %s · %s%s\n    %s", row.Title, row.Purpose, row.Run.Status, mark, row.SessionID)})
	}
	return m.startSelectorAt("Agents · select to view, Esc returns", options, selected, true, selectorAction{Kind: selectorAgent})
}

func (m *Model) openAgentView(id string) tea.Cmd {
	if m.routing == nil {
		return nil
	}
	if id == "root" {
		id = m.routing.rootID
	}
	if id == m.sessionID {
		return nil
	}
	m.routing.request++
	request := m.routing.request
	directory := m.routing.directory
	routing := m.routing
	return func() tea.Msg {
		client, err := directory.OpenReader(context.Background(), protocol.SessionID(id))
		if err != nil {
			return agentViewOpenedMsg{routing: routing, request: request, err: err}
		}
		view, err := client.Snapshot(context.Background())
		return agentViewOpenedMsg{routing: routing, request: request, client: client, snapshot: view, err: err}
	}
}

func (m *Model) parentAgentID() string {
	if m.routing == nil {
		return ""
	}
	for _, row := range m.routing.rows {
		if string(row.SessionID) == m.sessionID {
			return string(row.ParentSessionID)
		}
	}
	return m.routing.rootID
}

// agentSpawnOrder returns every child session in the tree ordered by spawn
// (CreatedAt, then BatchIndex, then SessionID for stability). The active view's
// position in this list drives both sibling cycling and the header ordinal.
func (m *Model) agentSpawnOrder() []protocol.ChildSession {
	if m.routing == nil || len(m.routing.rows) == 0 {
		return nil
	}
	rows := make([]protocol.ChildSession, len(m.routing.rows))
	copy(rows, m.routing.rows)
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		if rows[i].BatchIndex != rows[j].BatchIndex {
			return rows[i].BatchIndex < rows[j].BatchIndex
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	return rows
}

// agentOrdinal reports the active view's 1-based spawn position and the total
// number of sibling agents, for the "2/3" header label. It returns (0, total)
// when the root session is active.
func (m *Model) agentOrdinal() (int, int) {
	order := m.agentSpawnOrder()
	if len(order) == 0 {
		return 0, 0
	}
	for i, row := range order {
		if string(row.SessionID) == m.sessionID {
			return i + 1, len(order)
		}
	}
	return 0, len(order)
}

// cycleAgent moves the active view to the next (delta>0) or previous (delta<0)
// sibling agent in spawn order, wrapping at both ends. From the root it enters
// the first (or last) child. It is a no-op when there are no children.
func (m *Model) cycleAgent(delta int) tea.Cmd {
	order := m.agentSpawnOrder()
	if len(order) == 0 || delta == 0 {
		return nil
	}
	idx := -1
	for i, row := range order {
		if string(row.SessionID) == m.sessionID {
			idx = i
			break
		}
	}
	var target protocol.ChildSession
	switch {
	case idx < 0 && delta > 0:
		target = order[0]
	case idx < 0:
		target = order[len(order)-1]
	default:
		target = order[(idx+delta+len(order))%len(order)]
	}
	if string(target.SessionID) == m.sessionID {
		return nil
	}
	return m.openAgentView(string(target.SessionID))
}

// childSessionForCallID resolves the child sessions spawned by a Task/Agent
// tool call. A batch fan-out shares one ParentCallID and is distinguished by
// BatchIndex, so the result is ordered by BatchIndex then CreatedAt to render in
// input order. It returns nil when the directory is unavailable or the call has
// not (yet) produced children.
func (m *Model) childSessionForCallID(callID string) []protocol.ChildSession {
	if m.routing == nil || callID == "" {
		return nil
	}
	var matches []protocol.ChildSession
	for _, row := range m.routing.rows {
		if string(row.ParentCallID) == callID {
			matches = append(matches, row)
		}
	}
	if len(matches) < 2 {
		return matches
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].BatchIndex != matches[j].BatchIndex {
			return matches[i].BatchIndex < matches[j].BatchIndex
		}
		return matches[i].CreatedAt.Before(matches[j].CreatedAt)
	})
	return matches
}

// isAgentTool reports whether a tool name delegates to or inspects child
// sessions. These tools render a compact agent marker in the parent transcript
// instead of dumping the child's final answer as ordinary tool output.
func isAgentTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "task", "agent", "subagent":
		return true
	}
	return false
}

// attachAgentMarkers resolves the child sessions for every Task/Agent tool row
// in the composed transcript. It runs on compose and on each directory refresh
// so the marker's status and usage stay current while a child is running.
func (m *Model) attachAgentMarkers() {
	if m.routing == nil {
		return
	}
	for i := range m.items {
		item := &m.items[i]
		if item.kind != "tool" || !isAgentTool(item.toolName) {
			continue
		}
		item.agent = m.childSessionForCallID(item.toolID)
	}
}

// applyChildUpdate folds one multiplexed child event from the root stream into
// the directory cache, then refreshes the parent's agent markers. The runtime
// republishes each child's current session row as it runs, so the parent Task
// marker tracks live status and usage without waiting for the polling fallback.
// The child's nested transcript update is deliberately ignored: a child being
// viewed owns its own subscription, and child text must never enter the parent.
func (m *Model) applyChildUpdate(child protocol.ChildSession) {
	if m.routing == nil || child.SessionID == "" {
		return
	}
	// A terminal run cannot still own an actionable decision. Tolerate an
	// out-of-order/stale directory projection without leaving a phantom pending
	// badge in the parent after the child has completed.
	if !child.Run.Active() {
		child.Approval = nil
	}
	replaced := false
	for i := range m.routing.rows {
		if m.routing.rows[i].SessionID == child.SessionID {
			m.routing.rows[i] = child
			replaced = true
			break
		}
	}
	if !replaced {
		m.routing.rows = append(m.routing.rows, child)
	}
	m.attachAgentMarkers()
}

func childAwaitingApproval(child protocol.ChildSession) bool {
	return child.Approval != nil && child.Run.Status == "waiting_approval"
}

func (m *Model) startAgentSwitch() tea.Cmd {
	r := m.routing
	if r == nil || r.pending == nil || r.printing || r.switching {
		return nil
	}
	opened := *r.pending
	r.pending = nil
	fromChild := m.sessionID != r.rootID
	targetID := string(opened.snapshot.SessionID)
	toChild := targetID != "" && targetID != r.rootID
	// Already-dispatched print messages are joined before clearing the screen.
	// Not-yet-dispatched old-generation messages are discarded at admission.
	m.delivery.pending = nil
	r.generation = renderGeneration.Add(1)
	r.printScheduled = false
	r.printQueue = nil
	r.switching = true
	if m.watchCancel != nil {
		m.watchCancel()
	}
	if m.subscription != nil {
		_ = m.subscription.Close()
	}
	m.subscription = nil
	m.watchCancel = nil
	m.watchCtx = nil
	animation := m.spinner
	saved := *m
	saved.picker = nil
	saved.approval = nil
	r.views[m.sessionID] = saved
	r.drafts[m.sessionID] = sessionDraft{text: m.textarea.Value(), retry: m.retryDraft, retryID: m.retryCommandID, images: cloneInputImages(m.inputImages), retryImages: cloneInputImages(m.retryImages), missingImages: m.missingHistoryImages, follow: m.followOutput, offset: m.viewport.YOffset, paste: clonePasteFold(m.pasteFold)}
	for i, id := range r.order {
		if id == m.sessionID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.order = append(r.order, m.sessionID)
	for len(r.views) > 8 && len(r.order) > 0 {
		old := r.order[0]
		r.order = r.order[1:]
		if old != r.rootID && old != m.sessionID && r.views[old].pendingSubmissionCount() == 0 && r.views[old].pasteRequest == "" {
			view := r.views[old]
			r.drafts[old] = sessionDraft{text: view.textarea.Value(), retry: view.retryDraft, retryID: view.retryCommandID, images: cloneInputImages(view.inputImages), retryImages: cloneInputImages(view.retryImages), missingImages: view.missingHistoryImages, follow: view.followOutput, offset: view.viewport.YOffset, paste: clonePasteFold(view.pasteFold)}
			delete(r.views, old)
		}
	}
	host := m.terminal
	width, height := m.width, m.height
	ag := m.ag
	generation := m.watchGeneration + 1
	_, cached := r.views[string(opened.snapshot.SessionID)]
	if previous, ok := r.views[string(opened.snapshot.SessionID)]; ok {
		*m = previous
	} else {
		fresh := NewWithClient(nil, m.workspace, false)
		fresh.routing = r
		*m = fresh
	}
	m.ag = ag
	m.terminal = host
	m.delivery.pending = nil
	m.routing = r
	m.width = width
	m.height = height
	m.client = opened.client
	m.watchGeneration = generation
	m.watchCursor = protocol.Cursor{}
	m.spinner = animation
	m.watchCtx, m.watchCancel = context.WithCancel(context.Background())
	m.subscription = nil
	m.watchDisconnected = false
	m.watchRetry = 0
	m.hasSnapshot = false
	m.applySnapshot(opened.snapshot)
	// The root session owns Bubble Tea's ordinary screen and native scrollback.
	// Child sessions are deliberately managed in an alternate screen so their
	// output can never become part of the root transcript. Cached views retain
	// their own inline presentation identity and mode across navigation.
	m.inlineMode = !toChild
	if draft, ok := r.drafts[m.sessionID]; ok && !cached {
		m.textarea.SetValue(draft.text)
		m.retryDraft, m.retryCommandID = draft.retry, draft.retryID
		m.inputImages, m.retryImages = cloneInputImages(draft.images), cloneInputImages(draft.retryImages)
		m.pasteFold = clonePasteFold(draft.paste)
		m.missingHistoryImages = draft.missingImages
		m.followOutput = draft.follow
		m.viewport.SetYOffset(draft.offset)
	}
	if !cached {
		m.inline.forgetAll()
		m.inline.prime(m.items)
		m.inline.showInitialFrame()
	} else {
		// A cached view already owns the identity set for units admitted to
		// native scrollback. Re-priming it as a new baseline would print the
		// root history a second time when the user returns from a child.
		m.inline.ensure()
		m.inline.primed = true
		m.inline.showBaseline = false
	}
	m.layout()
	g := r.generation
	ack := func() tea.Msg { return agentViewResetMsg{generation: g} }
	switch {
	case !fromChild && toChild:
		return tea.Sequence(tea.EnterAltScreen, tea.ClearScreen, ack)
	case fromChild && !toChild:
		return tea.Sequence(tea.ExitAltScreen, ack)
	default:
		return tea.Sequence(tea.ClearScreen, ack)
	}
}

// Bubble Tea suspends its renderer while this trusted terminal reset runs.
// No subprocess is spawned and no worker is allowed to write to this stream.
type resetAgentTerminal struct{ output io.Writer }

func (c *resetAgentTerminal) SetStdin(io.Reader)    {}
func (c *resetAgentTerminal) SetStderr(io.Writer)   {}
func (c *resetAgentTerminal) SetStdout(w io.Writer) { c.output = w }
func (c *resetAgentTerminal) Run() error {
	// Clear only the managed alternate-screen frame. CSI 3J erases the
	// terminal's native scrollback, which belongs to the root conversation and
	// must survive child navigation.
	_, err := io.WriteString(c.output, "\x1b[2J\x1b[H")
	return err
}

func (m *Model) nextInlinePrint() tea.Cmd {
	r := m.routing
	if r == nil || r.printing || r.switching {
		return nil
	}
	// Once the read-only detail screen owns the terminal, completed root
	// transcript units must stay in the model until the reader returns. A
	// reader opened while a native print was already admitted sets
	// enterPending; that state is deliberately allowed to drain the existing
	// queue below before entering the alternate screen.
	if m.reader != nil && m.reader.screenEntered {
		return nil
	}
	if r.pending != nil {
		return m.startAgentSwitch()
	}
	for len(r.printQueue) > 0 {
		item := r.printQueue[0]
		r.printQueue = r.printQueue[1:]
		if item.generation != r.generation {
			continue
		}
		r.printing = true
		return m.printHistory(item)
	}
	if m.reader != nil && m.reader.enterPending {
		return m.enterReaderScreen()
	}
	return nil
}

func (m *Model) runAgentCommand(args []string) tea.Cmd {
	if m.routing == nil {
		return nil
	}
	if len(args) == 0 {
		return m.loadAgentCatalog(true)
	}
	if len(args) == 1 {
		return m.openAgentView(args[0])
	}
	// /agent <id> send|followup|interrupt|cancel|continue <text>
	id, action := protocol.SessionID(args[0]), args[1]
	control := protocol.AgentControl{ID: nextUICommandID(), SessionID: id, Action: action, Text: strings.Join(args[2:], " "), Strategy: protocol.InputSteer}
	if action == "followup" {
		control.Action = "send"
		control.Strategy = protocol.InputFollowup
	}
	var matches int
	for _, row := range m.routing.rows {
		if strings.HasPrefix(string(row.SessionID), string(id)) {
			matches++
			control.SessionID = row.SessionID
			control.RunID = row.Run.ID
		}
	}
	if matches != 1 {
		m.pushStatus("use an unambiguous session id from /agents")
		return nil
	}
	directory := m.routing.directory
	return func() tea.Msg {
		run, err := directory.Control(context.Background(), control)
		return agentControlMsg{id: id, run: run, err: err}
	}
}

func (m *Model) applyTranscript(items []protocol.TranscriptItem) {
	m.inline.nextIndex, m.inline.boundaryID = 0, ""
	converted := make([]historyCell, 0, len(items))
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		value := projectTranscriptCell(item)
		m.reconcileErrorReport(value)
		if item.PreviousID != "" {
			for _, old := range m.confirmedItems {
				if old.messageID != item.PreviousID {
					continue
				}
				m.inline.reconcile(old, value)
				if pending := m.delivery.pending; pending != nil && pending.next.epoch == m.inline.epoch {
					pending.next.reconcile(old, value)
				}
				break
			}
		}
		converted = append(converted, value)
	}
	m.confirmedItems = converted
	m.streaming = false
	for _, item := range items {
		if item.Kind == "assistant" && item.Status == "streaming" {
			m.streaming = true
		}
	}
	m.composeItems()
	m.renderVisibleTranscript()
}

func (m *Model) upsertTranscript(item protocol.TranscriptItem) {
	if item.ID == "" {
		return
	}
	value := projectTranscriptCell(item)
	if m.CellStore.replace(value) {
		if isAgentTool(value.toolName) {
			m.attachAgentMarkers()
		}
		return
	}
	for i := range m.confirmedItems {
		if m.confirmedItems[i].messageID == item.ID {
			m.confirmedItems[i] = value
			m.composeItems()
			return
		}
	}
	m.appendTranscript(value)
}

// projectTranscriptCell preserves protocol source and typed tool metadata.
// History and readers share this projection; no formatted text is reparsed.
func projectTranscriptCell(item protocol.TranscriptItem) historyCell {
	var args map[string]any
	if item.Kind == "tool" {
		args = protocolToolArgs(&protocol.ToolView{Name: item.Tool, Args: item.Args})
	}
	text := item.Text
	if item.Kind == "turn_summary" {
		label := "结束"
		switch item.Status {
		case "success":
			label = "完成"
		case "error":
			label = "失败"
		case "cancelled", "interrupted":
			label = "已停止"
		}
		const maxDurationMs = int64((1<<63 - 1) / int64(time.Millisecond))
		text = label + " · 用时 " + formatElapsed(time.Duration(min(item.DurationMs, maxDurationMs))*time.Millisecond)
	}
	return historyCell{kind: item.Kind, text: text, messageID: item.ID, turnID: item.TurnID,
		toolID: string(item.CallID), status: item.Status, toolName: item.Tool, toolArgs: args,
		toolArgsRaw: string(item.Args), toolTruncated: item.Truncated}
}
