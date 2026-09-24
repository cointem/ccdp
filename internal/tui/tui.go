// Package tui composes session, transcript, composer and panel state over a
// single acknowledged terminal output boundary. Presentation follows the
// Codex CLI cell/active-tail architecture with a Go/Bubble Tea backend.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

const appVersion = "0.2.0"

// clickTarget maps a line rendered in the viewport to an interactive target.
type clickTarget struct {
	line int
	kind string // "tool", "agent"
	id   string // toolID or sessionID
}

// Model is the root Bubble Tea model.
type Model struct {
	sessionHost
	composerState
	transcriptState
	panelState
	operationStore
	noticeStore
	terminal      *TerminalHost
	delivery      historyDelivery
	routing       *sessionRouting
	width, height int

	viewport viewport.Model
	spinner  spinner.Model

	client            protocol.SessionClient
	watchCtx          context.Context
	watchCancel       context.CancelFunc
	subscription      protocol.Subscription
	watchCursor       protocol.Cursor
	watchGeneration   uint64
	watchDisconnected bool
	watchRetry        int
	snapshot          protocol.SessionView
	hasSnapshot       bool
	commandSeq        uint64
	reportGeneration  uint64
	receiptKeys       map[protocol.CommandID]string
	// receiptOrder bounds the UI-only duplicate-display cache. Runtime command
	// idempotency remains authoritative in the protocol and is not affected by
	// evicting old receipt identities here.
	receiptOrder []protocol.CommandID
	// operationReports tracks typed operations whose bounded result should be
	// retained as a permanent report when the watch bridge emits a generic
	// tool-result event. It is deliberately UI-owned; the runtime remains the
	// source of command execution and receipts.
	operationReports map[protocol.CommandID]struct{}
	retryCommandID   protocol.CommandID
	retryDraft       string

	modelName    string
	mode         permissions.Mode
	planMode     bool
	tasksVisible bool // ctrl+t toggles the working task checklist panel
	sessionID    string
	workspace    string

	usage protocol.UsageSnapshot // token/cost accounting, updated on EventUsage

	showContextDetail bool
	detail            *readerState
	detailRow         int
	clickTargets      []clickTarget
	// linkTargets holds the clickable spans of rendered OSC 8 hyperlinks,
	// registered by render() alongside clickTargets. Because the TUI captures
	// mouse events, the terminal cannot turn a click on a hyperlink into
	// navigation; the transcript opens these destinations itself.
	linkTargets []linkTarget
	mouseX      int
	mouseY      int

	// selGrid holds each content row's ANSI-stripped visible text, aligned with
	// the row numbering used by clickTargets and mouse handlers. It feeds the
	// in-app drag selection (mirroring claude code's copy-on-select) so clicks
	// can still expand tool details while a drag copies text to the clipboard.
	selGrid   []string
	selAnchor *selPoint
	selFocus  *selPoint
	selActive bool
	selDrag   bool
	// selPressMsg is the raw press that may become either a click (acted on
	// release) or a drag-select, depending on whether motion occurred. Valid
	// only when selPressValid is set.
	selPressMsg   tea.MouseMsg
	selPressValid bool

	// copiedToast is a transient right-aligned hint ("copied N chars") drawn in
	// the always-present gap row above the composer. Unlike a status notice it
	// never adds a layout row, so the transcript does not shift when it appears
	// or expires. copiedToastSeq invalidates stale expiry ticks.
	copiedToast    string
	copiedToastSeq uint64

	// customCmds holds user-defined slash commands loaded from
	// ~/.ccdp/commands and <workspace>/.ccdp/commands.
	customCmds []customCommand

	// turnStarted is when the current turn began, for the completion bell.
	turnStarted time.Time

	// quitArmed latches the first ctrl+c press when idle: the second press
	// quits (Claude Code's two-stage exit), any other key disarms it.
	quitArmed bool

	// baseVpH is the transcript viewport height with no popup open; the
	// command-suggestion popup shrinks the viewport so the total screen
	// height stays constant.
	baseVpH int

	// followOutput stays true while the transcript is pinned to the bottom.
	// Scrolling upward disables it so streaming events do not yank the user's
	// viewport back down; returning to the bottom enables it again.
	followOutput bool

	// statusItems configures which items render in the header statusline
	// (/statusline). Valid tokens: version model mode session workspace cost.
	statusItems []string

	// autoResume opens the session picker on first render (ccdp -c).
	autoResume bool

	// reader is an explicit read-only detail surface. Its state is kept on the
	// session model so agent navigation can restore the composer and transcript
	// anchor without turning browsing into a transcript mutation.
	reader        *readerState
	readerSeq     uint64
	readerRequest uint64
}

type selectorActionKind string

const (
	selectorEffort      selectorActionKind = "effort"
	selectorVerbosity   selectorActionKind = "verbosity"
	selectorModel       selectorActionKind = "model"
	selectorMode        selectorActionKind = "mode"
	selectorPlan        selectorActionKind = "plan"
	selectorSandbox     selectorActionKind = "sandbox"
	selectorRewind      selectorActionKind = "rewind"
	selectorResume      selectorActionKind = "resume"
	selectorAgent       selectorActionKind = "agent"
	selectorAgentOutput selectorActionKind = "agent_output"
)

type selectorAction struct {
	Kind selectorActionKind
}

// defaultStatusItems is what the header shows unless /statusline changes it.
var defaultStatusItems = []string{"version", "model", "workspace", "mode", "cost", "context"}

type watchOpenedMsg struct {
	sub        protocol.Subscription
	sessionID  protocol.SessionID
	generation uint64
	err        error
}

type watchUpdateMsg struct {
	sub        protocol.Subscription
	update     protocol.Update
	sessionID  protocol.SessionID
	generation uint64
}

type watchClosedMsg struct {
	sub        protocol.Subscription
	sessionID  protocol.SessionID
	generation uint64
}

type commandReceiptMsg struct {
	receipt   protocol.Receipt
	sessionID protocol.SessionID
	purpose   string
}

// queryReportMsg carries a direct read-only query result. Query is an
// optional extension of SessionClient so older adapters can still use the
// typed CommandQuery fallback; the command/event path is handled separately
// below and remains authoritative when no direct method is available.
type queryReportMsg struct {
	commandID protocol.CommandID
	report    protocol.QueryReport
	sessionID protocol.SessionID
	err       error
}

type commandErrorMsg struct {
	commandID protocol.CommandID
	sessionID protocol.SessionID
	err       error
}

type customCommandLoadedMsg struct {
	command    customCommand
	args       []string
	session    string
	generation uint64
	data       []byte
	err        error
}

// errMsg is a local error surfaced to the UI.
type errMsg struct{ err error }

// initResumeMsg triggers the saved-session picker after the first Update.
// Init runs on a value copy of the model (opening the picker there would
// mutate the copy and be lost), so it schedules this message and Update —
// whose returned model is the persistent state — performs the actual work.
type initResumeMsg struct{}

// NewWithClient creates a TUI over the narrow application protocol. It is
// useful for tests and for future app/session adapters that do not expose an
// *agent.Agent. No command or event channel is needed by this constructor.
func NewWithClient(client protocol.SessionClient, workspace string, autoResume bool) Model {
	ta := newTextarea()

	vp := viewport.New(80, 20)
	vp.Style = lipgloss.NewStyle().Padding(0, 1)

	sp := spinner.New()
	sp.Style = styleRunning
	sp.Spinner = spinner.Spinner{Frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, FPS: time.Second / 10}

	m := Model{
		client:          client,
		viewport:        vp,
		composerState:   composerState{textarea: ta},
		spinner:         sp,
		workspace:       workspace,
		autoResume:      autoResume,
		followOutput:    true,
		transcriptState: transcriptState{inlineMode: true},
		statusItems:     append([]string(nil), defaultStatusItems...),
	}
	m.watchCtx, m.watchCancel = context.WithCancel(context.Background())
	if client != nil {
		if snapshot, err := client.Snapshot(context.Background()); err == nil {
			m.applySnapshot(snapshot)
		} else {
			m.pushStatus("session snapshot unavailable: " + err.Error())
		}
	}
	if workspace != "" {
		m.customCmds = loadCustomCommands(workspace)
	}

	// Greet only a fresh protocol session (and only after a successful
	// snapshot).
	if client != nil && !autoResume && (!m.hasSnapshot || len(m.snapshot.History) == 0 && len(m.snapshot.Transcript) == 0) {
		m.addReport(welcomeItem(workspace))
	}
	// The initial frame owns the baseline history. It should not be emitted a
	// second time when the first live turn is flushed to native scrollback.
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	return m
}

// Init starts the event watcher and spinner, and titles the terminal window.
// With autoResume set (ccdp -c) it schedules initResumeMsg; the picker must be
// opened from Update because Init receives a value copy of the model.
func (m Model) Init() tea.Cmd {
	// The managed transcript owns clicks; an in-app drag selection copies the
	// highlighted text to the clipboard on release (claude code's copy-on-select),
	// so native Shift-drag is not required to copy what is on screen.
	cmds := []tea.Cmd{restoreMouseCmd(), m.openWatchCmd(), m.spinner.Tick, noticeTickCmd(), tea.SetWindowTitle("ccdp — " + m.workspace)}
	if m.routing != nil {
		// Seed the directory once for children that already exist on attach;
		// live changes thereafter arrive as ChildUpdate events on the root
		// stream, so there is no periodic catalog poll.
		cmds = append(cmds, m.loadAgentCatalog(false))
	}
	if m.autoResume {
		cmds = append(cmds, func() tea.Msg { return initResumeMsg{} })
	}
	return tea.Batch(cmds...)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	// Every event can change wrapped chrome: receipts, expiry, and catalog
	// updates do not necessarily change input height or terminal dimensions.
	// Size the returned model after handling the event so scrolling and View
	// use the same body budget on the very next frame.
	switch updated := next.(type) {
	case Model:
		updated.syncViewportHeight()
		if updated.followOutput {
			updated.viewport.GotoBottom()
		}
		return updated, cmd
	case *Model:
		updated.syncViewportHeight()
		if updated.followOutput {
			updated.viewport.GotoBottom()
		}
	}
	return next, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case terminalErrorMsg:
		m.pushStatus("terminal output: " + msg.err.Error())
		return m, tea.Quit
	case reconnectMsg:
		if msg.sessionID != m.sessionID || msg.generation != m.watchGeneration || !m.watchDisconnected {
			return m, nil
		}
		return m, m.openWatchCmd()
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case effortTickMsg:
		return m, m.advanceEffortMotion(msg)
	case clipboardReadMsg:
		m.routeClipboardRead(msg)
		return m, nil
	case linkOpenMsg:
		// Launching is best-effort: a missing opener (no xdg-open) surfaces as a
		// notice instead of failing the click silently.
		if msg.err != nil {
			m.pushStatus("open " + msg.target + ": " + msg.err.Error())
		}
		return m, nil
	case agentCatalogMsg:
		if m.routing == nil || msg.directory != m.routing.directory {
			return m, nil
		}
		m.routing.catalogLoading = false
		if msg.err != nil {
			if msg.show {
				m.pushStatus(msg.err.Error())
			}
			return m, nil
		}
		m.routing.rows = msg.rows
		m.attachAgentMarkers()
		var command tea.Cmd
		if msg.show {
			command = m.agentPicker()
		}
		if m.subscription == nil && m.sessionID != m.routing.rootID && !m.routing.switching {
			command = tea.Batch(command, m.openWatchCmd())
		}
		return m, command
	case agentViewOpenedMsg:
		if m.routing == nil || msg.routing != m.routing || msg.request != m.routing.request {
			return m, nil
		}
		if msg.err != nil {
			m.pushStatus(msg.err.Error())
			// Selector mouse tracking is not released by a command receipt;
			// the agent view never reaches the completion reset below.
			releaseMouse := m.mouseReleasePending
			m.mouseReleasePending = false
			return m, boolCmd(releaseMouse, restoreMouseCmd())
		}
		m.routing.pending = &msg
		return m, m.startAgentSwitch()
	case agentViewResetMsg:
		if m.routing == nil || msg.generation != m.routing.generation {
			return m, nil
		}
		m.routing.switching = false
		if msg.err != nil {
			m.pushStatus("terminal reset: " + msg.err.Error())
		}
		if m.routing.pending != nil {
			return m, m.startAgentSwitch()
		}
		// Choosing a session from /agents picks a selector, not a command
		// receipt. Release its mouse tracking now that the view switch has
		// settled; otherwise the terminal can no longer scroll native
		// scrollback until the next command completes.
		releaseMouse := m.mouseReleasePending
		m.mouseReleasePending = false
		// The root watch is paused while a child owns the alternate screen, so
		// terminal child updates may have happened while no root subscriber was
		// attached. Refresh the lightweight directory on every settled switch;
		// otherwise a completed child can leave a stale approval badge behind.
		return m, tea.Batch(m.openWatchCmd(), m.loadAgentCatalog(false), m.flushInline(), boolCmd(releaseMouse, restoreMouseCmd()))
	case inlinePrintMsg:
		if m.routing == nil || msg.generation != m.routing.generation {
			return m, nil
		}
		m.routing.printScheduled = false
		m.routing.printQueue = append(m.routing.printQueue, msg)
		return m, m.nextInlinePrint()
	case inlinePrintedMsg:
		if m.routing == nil || msg.generation != m.routing.generation {
			return m, nil
		}
		pending := m.delivery.pending
		if pending == nil || pending.id != msg.batch || pending.generation != msg.generation {
			return m, nil
		}
		if err := m.acknowledgeHistory(msg); err != nil {
			m.pushStatus(err.Error())
			return m, tea.Quit
		}
		m.routing.printing = false
		return m, tea.Batch(m.nextInlinePrint(), m.flushInline())
	case agentControlMsg:
		if msg.err != nil {
			m.pushStatus("agent control: " + msg.err.Error())
		} else {
			m.pushStatus(fmt.Sprintf("agent %s · %s", msg.id, msg.run.Status))
		}
		return m, m.loadAgentCatalog(false)
	case agentHistoryMsg:
		return m, m.showAgentHistory(msg)
	case agentOutputMsg:
		return m, tea.Batch(m.showAgentOutput(msg), m.flushInline())
	case noticeTickMsg:
		m.expireNotices(msg.at)
		if len(m.notices) > 0 {
			return m, noticeTickCmd()
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		// The first real size is the earliest point at which wrapping is
		// trustworthy. Commit resumed history here so a tall baseline enters
		// scrollback at the terminal's actual width before the live tail trims.
		if m.inlineMode && m.inline.showBaseline {
			cmds = append(cmds, m.flushInline())
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		model, cmd := m.handleKey(msg)
		return appendInlineFlush(model, cmd)

	case watchOpenedMsg:
		return m.handleWatchOpened(msg)

	case watchUpdateMsg:
		wasModal := m.question != nil || m.approval != nil || m.picker != nil
		model, cmd := m.handleWatchUpdate(msg)
		return model, tea.Batch(cmd, modalMouseTransition(wasModal, model))

	case watchClosedMsg:
		return m.handleWatchClosed(msg)

	case commandReceiptMsg:
		wasModal := m.question != nil || m.approval != nil || m.picker != nil
		if msg.sessionID == "" || msg.sessionID.String() == m.sessionID {
			m.applyReceipt(msg.receipt, msg.purpose)
		} else if m.routing != nil {
			if saved, ok := m.routing.views[string(msg.sessionID)]; ok {
				saved.applyReceipt(msg.receipt, msg.purpose)
				m.routing.views[string(msg.sessionID)] = saved
			}
		}
		releaseMouse := m.mouseReleasePending
		m.mouseReleasePending = false
		return m, tea.Batch(modalMouseTransition(wasModal, &m), boolCmd(releaseMouse, restoreMouseCmd()))

	case queryReportMsg:
		if msg.sessionID == "" || msg.sessionID.String() == m.sessionID {
			m.applyQueryReport(msg)
		}
		return m, m.flushInline()

	case commandErrorMsg:
		if m.routing != nil && msg.sessionID != "" && string(msg.sessionID) != m.sessionID {
			if saved, ok := m.routing.views[string(msg.sessionID)]; ok {
				delete(saved.operationReports, msg.commandID)
				saved.restoreFailedSubmission(msg.commandID)
				if msg.err != nil {
					saved.pushStatus("submit failed: " + msg.err.Error())
				}
				m.routing.views[string(msg.sessionID)] = saved
			}
		}
		if msg.sessionID == "" || msg.sessionID.String() == m.sessionID {
			if op, ok := m.operation(msg.commandID); ok {
				m.updateOperation(msg.commandID, OperationFailed, protocol.Receipt{CommandID: msg.commandID, SessionID: msg.sessionID,
					Error: &protocol.CommandError{Code: protocol.ErrorInternal, Message: errorText(msg.err)}})
				if m.operationOwnsPresentation(msg.commandID) {
					m.finishActivity(msg.commandID, ActivityFailed, errorText(msg.err))
					m.clearNotice(msg.commandID)
					m.addNotice(Notice{CommandID: msg.commandID, Severity: NoticeError, Text: "submit failed: " + errorText(msg.err), Sticky: true})
				} else if op.Active() {
					// The failed operation still closes below, but its error is not
					// promoted over a newer command's activity.
				}
			}
			delete(m.operationReports, msg.commandID)
			if m.question != nil && m.question.pending == msg.commandID {
				m.question.pending = ""
			}
			// Submit errors have no receipt that can clear the in-flight
			// marker. Move the draft to the explicit retry slot before deleting
			// the marker; otherwise resume remains blocked forever after a
			// transport/session failure.
			m.restoreFailedSubmission(msg.commandID)
			if m.approvalPending && msg.commandID == m.pendingApprovalCommand {
				m.approvalPending = false
				m.pendingApprovalCommand = ""
			}
			if msg.err != nil {
				if _, ok := m.operation(msg.commandID); !ok {
					m.pushStatus("submit failed: " + msg.err.Error())
				}
			}
		}
		releaseMouse := m.mouseReleasePending
		m.mouseReleasePending = false
		return m, boolCmd(releaseMouse, restoreMouseCmd())

	case customCommandLoadedMsg:
		if (msg.session != "" && msg.session != m.sessionID) || msg.generation != m.reportGeneration {
			return m, nil
		}
		if msg.err != nil {
			m.pushLog("error", "/"+msg.command.name+": "+msg.err.Error())
			return m, nil
		}
		text := expandCustomCommand(msg.command, msg.args, msg.data)
		input := protocol.NewSubmitInput(nextUICommandID(), protocol.SessionID(m.sessionID), protocol.InputID(nextUICommandID()), text, protocol.InputSteer)
		return m, m.submitCommand(input, "custom command submitted")

	case clipboardResultMsg:
		if (msg.sessionID != "" && msg.sessionID != m.sessionID) || msg.generation != m.reportGeneration {
			return m, nil
		}
		if msg.err != nil {
			m.pushStatus("copy failed: " + msg.err.Error())
			return m, nil
		}
		if msg.selection {
			// Drag copies take the right edge of the hint row; a status notice
			// would overwrite the left-side lane content.
			return m, m.showCopiedToast(msg.chars)
		}
		m.pushStatus("copied to clipboard")
		return m, nil

	case copiedToastExpireMsg:
		if msg.seq == m.copiedToastSeq {
			m.copiedToast = ""
		}
		return m, nil

	case resumeOpenedMsg:
		wasModal := m.question != nil || m.approval != nil || m.picker != nil
		model, cmd := m.handleResumeOpened(msg)
		// Resume completes outside the command-receipt path, so it must consume
		// the selector's pending mouse release itself; otherwise mouse tracking
		// stays enabled after the picker closes and the terminal can no longer
		// scroll its native scrollback.
		releaseMouse := m.mouseReleasePending
		m.mouseReleasePending = false
		return model, tea.Batch(cmd, modalMouseTransition(wasModal, model), boolCmd(releaseMouse, restoreMouseCmd()))

	case resumeCandidateClosedMsg:
		return m, nil

	case spinner.TickMsg:
		// Spinner ticks continue while the TUI is idle, so they also provide a
		// bounded expiry path for a notice added after the dedicated notice tick
		// stopped at an empty queue.
		m.expireNotices(now())
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.animateWork() {
			m.render()
		}
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case errMsg:
		m.pushStatus("error: " + msg.err.Error())
		return m, nil

	case initResumeMsg:
		m.autoResume = false // only once
		return m, m.startResumePicker()

	case editorFinishedMsg:
		m.closeExternalEditor(msg)
		return m, nil

	}

	// Default: forward to textarea (keeps cursor behavior live).
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

const maxInputLines = 5
const maxPickerRows = 10

// maxCmdSuggestions caps only the visible command window. All matches remain
// selectable with up/down, page keys, home and end.
const maxCmdSuggestions = 8

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func removeString(values []string, want string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != want {
			out = append(out, value)
		}
	}
	return out
}

func restoreMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.EnableMouseCellMotion() }
}

func modalMouseTransition(wasModal bool, model tea.Model) tea.Cmd {
	return nil
}

func boolCmd(enabled bool, cmd tea.Cmd) tea.Cmd {
	if enabled {
		return cmd
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func (m *Model) pushStatus(s string) {
	m.addNotice(Notice{Text: s})
}

func (m *Model) toggleToolExpanded(toolID string) {
	if m.detail != nil && m.detail.itemID == toolID {
		m.detail = nil
		return
	}
	for i := range m.items {
		item := &m.items[i]
		id := detailIdentity(item, i)
		if id != toolID || (item.kind == "thinking" && thinkingInProgress(item)) {
			continue
		}
		text := m.detailContent(item)
		m.detailRow = presentationHeight(m.headerPresentation(), m.width)
		for _, target := range m.clickTargets {
			if target.id == toolID && (target.kind == "tool" || target.kind == "thought") {
				m.detailRow = max(m.detailRow, presentationHeight(m.headerPresentation(), m.width)+target.line-m.viewport.YOffset)
				break
			}
		}
		m.detail = &readerState{itemID: toolID, text: text, rawText: text}
		return
	}
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	m.mouseX = msg.X
	m.mouseY = msg.Y
	if m.detail != nil {
		if msg.Button == tea.MouseButtonWheelUp {
			m.moveDetail(-3)
			return *m, nil
		}
		if msg.Button == tea.MouseButtonWheelDown {
			m.moveDetail(3)
			return *m, nil
		}
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			m.detail = nil
			return *m, nil
		}
	}
	// A popup owns wheel input; never scroll the transcript behind it.
	if m.overlaySurface() && (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		key := tea.KeyDown
		if msg.Button == tea.MouseButtonWheelUp {
			key = tea.KeyUp
		}
		if m.picker != nil || m.approvalActive() || m.questionActive() {
			return m.handleKey(tea.KeyMsg{Type: key})
		}
		return *m, nil
	}

	// Mouse wheel up/down scrolls the transcript viewport
	if msg.Button == tea.MouseButtonWheelUp {
		if m.inlineMode && m.followOutput {
			m.render()
		}
		m.followOutput = false
		m.viewport.LineUp(3)
		return *m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		m.viewport.LineDown(3)
		m.followOutput = m.viewport.AtBottom()
		return *m, nil
	}

	// Left-button press/motion/release drives an in-app drag selection that
	// copies on release, mirroring claude code's copy-on-select. A press that
	// never drags collapses back to a normal click (expand tool detail, toggle
	// context, switch model, etc.), so mouse clicks keep working while a drag
	// copies text — because that app-level copy never captured the native
	// terminal drag-selection gesture.
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return *m, nil
		}
		if m.overlaySurface() {
			return m.doMouseClick(msg)
		}
		headerH := presentationHeight(m.headerPresentation(), m.width)
		inBody := msg.Y >= headerH && msg.Y < headerH+m.viewport.Height
		if !inBody {
			return m.doMouseClick(msg)
		}
		m.selPressMsg = msg
		m.selPressValid = true
		m.selDrag = false
		m.selActive = false
		// Anchor at the press cell itself; the first motion event already carries
		// a moved coordinate, so anchoring there would drift the selection start.
		a := m.selPointAt(msg.Y, msg.X)
		m.selAnchor = &a
		m.selFocus = nil
		return *m, nil

	case tea.MouseActionMotion:
		if !m.selPressValid {
			return *m, nil
		}
		cp := m.selPointAt(msg.Y, msg.X)
		if m.selAnchor == nil {
			a := cp
			m.selAnchor = &a
		}
		anchor := *m.selAnchor
		if !m.selDrag && (cp.Row != anchor.Row || cp.Col != anchor.Col) {
			m.selDrag = true
		}
		m.selFocus = &cp
		if m.selDrag {
			m.selActive = true
		}
		return *m, nil

	case tea.MouseActionRelease:
		if msg.Button != tea.MouseButtonLeft || !m.selPressValid {
			return *m, nil
		}
		drag, anchor, focus := m.selDrag, m.selAnchor, m.selFocus
		press := m.selPressMsg
		m.selPressValid = false
		m.selDrag = false
		if drag && anchor != nil && focus != nil {
			if text := selectionText(m.selGrid, *anchor, *focus); text != "" && strings.TrimSpace(text) != "" {
				// Keep the highlight so the user sees exactly what was copied:
				// the anchor/focus stay on the model until the next press.
				m.selActive = true
				return *m, m.copySelectionCmd(text)
			}
			m.selActive = false
			m.selAnchor = nil
			m.selFocus = nil
			return *m, nil
		}
		m.selActive = false
		m.selAnchor = nil
		m.selFocus = nil
		return m.doMouseClick(press)
	}
	return *m, nil
}

// selPointAt converts a screen coordinate to a transcript content cell, using
// the same header/viewport geometry as the click and selection handlers.
func (m *Model) selPointAt(screenY, screenX int) selPoint {
	headerH := presentationHeight(m.headerPresentation(), m.width)
	row := m.viewport.YOffset + (screenY - headerH)
	col := screenX - m.viewport.Style.GetHorizontalFrameSize()/2
	return selPoint{Row: row, Col: max(0, col)}
}

// doMouseClick executes the immediate action bound to a left click: footer
// model/mode/context toggles, context-card dismissal, header agent navigation,
// and transcript tool/agent row expansion.
func (m *Model) doMouseClick(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	for _, hit := range m.footerHitCells() {
		if msg.Y != hit.y || msg.X < hit.x || msg.X >= hit.x+hit.width {
			continue
		}
		switch hit.kind {
		case "model", "permission":
			kind, command := selectorModel, "/model"
			if hit.kind == "permission" {
				kind, command = selectorMode, "/mode"
			}
			m.showContextDetail = false
			if m.picker != nil && m.picker.action.Kind == kind {
				m.picker = nil
				m.syncViewportHeight()
				return *m, nil
			}
			return m.runCommand(command)
		case "context", "cache":
			m.picker = nil
			m.showContextDetail = !m.showContextDetail
			m.syncViewportHeight()
			return *m, nil
		}
	}
	// Clicking elsewhere dismisses the context card.
	if m.showContextDetail {
		m.showContextDetail = false
		return *m, nil
	}

	if m.activePanel() != panelNone {
		return *m, nil
	}
	headerH := presentationHeight(m.headerPresentation(), m.width)
	if msg.Y < headerH && m.routing != nil {
		if m.sessionID != m.routing.rootID {
			return *m, m.openAgentView(m.parentAgentID())
		}
		return *m, m.openAgentPicker()
	}

	// Click inside viewport (transcript):
	if msg.Y < headerH || msg.Y >= headerH+m.viewport.Height {
		return *m, nil
	}
	clickedLine := m.viewport.YOffset + (msg.Y - headerH)
	// A hyperlink label wins over the row's other click targets: the cell under
	// the pointer commits to opening its destination.
	clickedCol := msg.X - m.viewport.Style.GetHorizontalFrameSize()/2
	for _, link := range m.linkTargets {
		if link.row == clickedLine && clickedCol >= link.start && clickedCol < link.end {
			return *m, m.openLinkCmd(link.target)
		}
	}
	for _, target := range m.clickTargets {
		if target.line == clickedLine {
			switch target.kind {
			case "tool", "thought":
				m.toggleToolExpanded(target.id)
				return *m, nil
			case "agents":
				return *m, m.openAgentPicker()
			case "agent":
				return *m, m.openAgentView(target.id)
			}
		}
	}

	return *m, nil
}
