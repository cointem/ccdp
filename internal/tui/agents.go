package tui

import (
	"context"
	"fmt"
	"io"
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

type agentCatalogTick struct{ directory protocol.SessionDirectory }
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
	generation uint64
	text       string
}
type inlinePrintedMsg struct{ generation uint64 }

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
	m.routing = &sessionRouting{directory: directory, rootID: m.sessionID, rootClient: m.client, views: make(map[string]Model), drafts: make(map[string]sessionDraft), generation: renderGeneration.Add(1)}
}

func agentCatalogTickCmd(directory protocol.SessionDirectory) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return agentCatalogTick{directory: directory} })
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

func (m *Model) agentPicker() tea.Cmd {
	options := []selectorOption{{ID: m.routing.rootID, Label: "main · " + m.routing.rootID}}
	selected := 0
	for _, row := range m.routing.rows {
		mark := ""
		if row.Approval != nil {
			mark = " · approval required"
		}
		if string(row.SessionID) == m.sessionID {
			selected = len(options)
		}
		options = append(options, selectorOption{ID: string(row.SessionID), Label: fmt.Sprintf("%s · %s · %s%s\n    %s", row.Title, row.Purpose, row.Run.Status, mark, row.SessionID)})
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
		if old != r.rootID && old != m.sessionID && len(r.views[old].pendingSubmissions) == 0 && r.views[old].pasteRequest == "" {
			view := r.views[old]
			r.drafts[old] = sessionDraft{text: view.textarea.Value(), retry: view.retryDraft, retryID: view.retryCommandID, images: cloneInputImages(view.inputImages), retryImages: cloneInputImages(view.retryImages), missingImages: view.missingHistoryImages, follow: view.followOutput, offset: view.viewport.YOffset, paste: clonePasteFold(view.pasteFold)}
			delete(r.views, old)
		}
	}
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
	m.routing = r
	m.width = width
	m.height = height
	m.client = opened.client
	m.watchGeneration = generation
	m.watchCursor = protocol.Cursor{}
	m.watchCtx, m.watchCancel = context.WithCancel(context.Background())
	m.subscription = nil
	m.approvalPending = false
	m.pendingApprovalCommand = ""
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
		return tea.Sequence(tea.Println(item.text), func() tea.Msg { return inlinePrintedMsg{generation: item.generation} })
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
	converted := make([]logItem, 0, len(items))
	for _, item := range items {
		value := transcriptLogItem(item)
		m.reconcileErrorReport(value)
		if item.PreviousID != "" {
			// A stream gains its durable message ID at commit. Preserve print
			// acknowledgements only when the final text is identical; changed
			// provider output must still be shown in full. A cumulative stream can
			// grow between the provisional and durable rows, so carry its emitted
			// prefix offset separately when that prefix is still byte-for-byte
			// stable.
			for i, old := range m.confirmedItems {
				if old.messageID != item.PreviousID {
					continue
				}
				oldID := itemIdentity(old, i)
				newItem := logItem{messageID: item.ID}
				newID := itemIdentity(newItem, 0)
				m.inline.ensure()
				if old.text == value.text {
					if _, ok := m.inline.printed[oldID]; ok {
						m.inline.printed[newID] = struct{}{}
					}
				}
				oldKey := inlineLogicalKey(old, i)
				newKey := inlineLogicalKey(newItem, 0)
				if offset, ok := m.inline.offsets[oldKey]; ok {
					// Offsets are byte indexes into the original source. Never
					// apply one to a corrected prefix; letting the new durable
					// row render from zero is safer than dropping changed text.
					offset = min(max(0, offset), len(old.text))
					if offset <= len(value.text) && strings.HasPrefix(value.text, old.text[:offset]) {
						m.inline.offsets[newKey] = offset
					}
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
	m.render()
}

func (m *Model) upsertTranscript(item protocol.TranscriptItem) {
	value := transcriptLogItem(item)
	for i := range m.confirmedItems {
		if m.confirmedItems[i].messageID == item.ID {
			m.confirmedItems[i] = value
			m.composeItems()
			return
		}
	}
	m.appendTranscript(value)
}

// transcriptLogItem is the single projection used by snapshot and live
// transcript paths. Tool text is formatted through renderToolText, keeping
// the displayed summary identical for both paths; the original structured
// fields remain on logItem for the detail reader and presentation layer.
func transcriptLogItem(item protocol.TranscriptItem) logItem {
	text, meta := item.Text, false
	args := map[string]any(nil)
	if item.Kind == "tool" {
		args = protocolToolArgs(&protocol.ToolView{Name: item.Tool, Args: item.Args})
		text, meta = renderToolText(item.Tool, args, item.Status, item.Text)
	}
	if item.Truncated {
		text += "\n[preview truncated; /transcript for saved output]"
	}
	return logItem{kind: item.Kind, text: text, messageID: item.ID, turnID: item.TurnID, toolID: string(item.CallID), status: item.Status,
		toolMeta: meta, toolName: item.Tool, toolArgs: args, toolArgsRaw: string(item.Args), toolOutput: item.Text, toolTruncated: item.Truncated}
}
