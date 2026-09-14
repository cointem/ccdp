// Package tui implements the terminal user interface for ccdp. It follows
// Claude Code's interactive chat model (streaming assistant text, permission
// modals, slash commands) rendered with Bubble Tea.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/agent"
	"ccdp/internal/commands"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

const appVersion = "0.2.0"

// logItem is one rendered entry in the conversation viewport.
type logItem struct {
	kind   string // user | command | assistant | system | welcome | tool | status | error | thinking
	text   string
	toolID string
	status string // running | success | error | denied
	// messageID is set for confirmed history entries. Local report entries use
	// reportAnchor/reportIndex to retain their position when a later snapshot
	// rebuilds the confirmed transcript.
	messageID string
	turnID    protocol.TurnID // associates transient errors with their durable turn outcome
	// reportID is a process-local stable identity for a local command/report.
	// It is separate from runtime message IDs so a report cannot collide with a
	// server-owned transcript message.
	reportID     string
	reportAnchor string
	reportIndex  int

	// tool entries: whether the second line of text is the command/path/args
	// meta line (colored at render time, after sanitization).
	toolMeta bool
	// Typed tool source is retained alongside the compact display text.  The
	// presentation layer can therefore summarize a tool without reparsing the
	// formatted string, while legacy fixtures that only populate text continue
	// to use the fallback parser.
	toolName      string
	toolArgs      map[string]any
	toolArgsRaw   string
	toolOutput    string
	toolTruncated bool

	// sanitizeANSI display cache (valid while sanitizedLen == len(text)).
	sanitized    string
	sanitizedLen int
}

// Model is the root Bubble Tea model.
type Model struct {
	routing       *sessionRouting
	width, height int

	viewport viewport.Model
	textarea textarea.Model
	spinner  spinner.Model

	ag *agent.Agent
	// client is the sole mutation/read boundary used by production TUI code.
	// ag remains the concrete session-lifecycle handle used for opening/listing
	// sessions and final shutdown after a resume swap; it is not an event or
	// business-operation adapter.
	client           protocol.SessionClient
	watchCtx         context.Context
	watchCancel      context.CancelFunc
	subscription     protocol.Subscription
	watchCursor      protocol.Cursor
	watchGeneration  uint64
	snapshot         protocol.SessionView
	hasSnapshot      bool
	commandSeq       uint64
	reportGeneration uint64
	// operations owns command lifecycle state. pendingSubmissions is retained
	// as the draft/attachment compatibility projection for existing adapters.
	operations         map[protocol.CommandID]Operation
	operationSeq       uint64
	latestOperation    protocol.CommandID
	pendingSubmissions map[protocol.CommandID]string
	receiptKeys        map[protocol.CommandID]string
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

	items []logItem // conversation log
	// confirmedItems mirrors the latest runtime history projection. reports
	// are local command/diagnostic output and survive history snapshots; keeping
	// the layers separate prevents a resync or clear confirmation from erasing
	// useful UI evidence.
	confirmedItems []logItem
	reports        []logItem
	// inline owns only terminal presentation state. Runtime history remains in
	// confirmedItems/reports; the ordinary-screen renderer prints completed
	// units through tea.Println and keeps active output bounded in the frame.
	inline             inlineTranscript
	inlineMode         bool
	focus              FocusRouter
	transcriptScroll   ScrollState
	streaming          bool // an assistant message is being streamed
	turnDone           bool // the latest turn reached a terminal event/snapshot
	turnFailed         bool // latest turn ended with an error/cancellation
	interruptRequested bool
	busy               bool
	status             string
	activity           Activity
	notices            []Notice
	pendingInputs      []protocol.InputView

	question        *questionState
	approval        *agent.ApprovalRequest
	approvalPending bool
	// mouseReleasePending is set when a selector closes immediately before a
	// protocol command. The receipt/error path emits DisableMouse after the
	// command result so direct command callers still receive the typed receipt.
	mouseReleasePending bool
	// pendingApprovalCommand identifies the one approval decision currently
	// awaiting a receipt. A receipt for an unrelated command must never close
	// the modal while commands complete out of order on the watch.
	pendingApprovalCommand protocol.CommandID
	// approvalScroll keeps long commands/plans reviewable inside the modal.
	approvalScroll int
	approvalState  ScrollState

	modelName            string
	mode                 permissions.Mode
	planMode             bool
	sessionID            string
	workspace            string
	clipboard            Clipboard
	clipboardReader      ClipboardReader
	pasteRequest         protocol.CommandID
	inputImages          []protocol.InputImage
	selectedImage        int
	pendingImages        map[protocol.CommandID][]protocol.InputImage
	retryImages          []protocol.InputImage
	historyImages        map[int][]protocol.InputImage
	expiredHistoryImages map[int]bool
	missingHistoryImages bool

	usage agent.Usage // token/cost accounting, updated on EventUsage

	history    []string
	historyIdx int

	// picker drives scrollable list selection (e.g. /rewind and /resume).
	picker *pickerState

	// cmdSug is the live slash-command autocomplete list (popup above the
	// input) while the user is typing a command name after "/". cmdSugIdx is
	// the highlighted entry; navigation covers the complete result set while
	// the popup renders a small window around the selection.
	cmdSug    []string
	cmdSugIdx int

	// customCmds holds user-defined slash commands loaded from
	// ~/.ccdp/commands and <workspace>/.ccdp/commands.
	customCmds []customCommand

	modalErr string // transient message inside the modal area

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

	// pasteFold retains the original long paste while its compact preview is
	// shown near the composer. The underlying textarea always remains editable.
	pasteFold *pasteFoldState

	// resumePending gates new submissions while an independent session handle
	// is being opened. This prevents a late successful open from closing the
	// handle that accepted a newly submitted turn.
	resumePending bool
	resumeTask    *resumeTaskState
	retiredAgents []*agent.Agent
}

// pickerState is a lightweight numbered-list selector.
type pickerState struct {
	title        string
	lines        []string
	options      []selectorOption
	selector     Selector
	action       selectorAction
	effortMotion effortMotion
	buf          string // digits typed so far
	index        int    // keyboard-highlighted item
	inline       bool   // render immediately above the composer
}

type selectorOption = SelectorOption

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

// resumeOpenedMsg is produced after an independent Agent handle has been
// opened. The source scope is carried through the asynchronous operation so a
// late result cannot replace a session that the user has already changed.
type resumeOpenedMsg struct {
	candidate          *agent.Agent
	snapshot           protocol.SessionView
	source             *agent.Agent
	task               *resumeTaskState
	expectedSessionID  string
	expectedGeneration uint64
	err                error
}

type resumeCandidateClosedMsg struct{}

// resumeTaskState outlives Bubble Tea's value Model copies. It lets shutdown
// join an OpenSession operation that has started, or cancel a command that was
// queued but never run, and close an unclaimed candidate exactly once.
type resumeTaskState struct {
	mu        sync.Mutex
	done      chan struct{}
	started   bool
	canceled  bool
	finished  bool
	claimed   bool
	candidate *agent.Agent
}

// errMsg is a local error surfaced to the UI.
type errMsg struct{ err error }

// initResumeMsg triggers the saved-session picker after the first Update.
// Init runs on a value copy of the model (opening the picker there would
// mutate the copy and be lost), so it schedules this message and Update —
// whose returned model is the persistent state — performs the actual work.
type initResumeMsg struct{}

// newTextarea builds the input textarea with ccdp's key bindings: Enter
// submits (handled by handleKey), Alt+Enter / Ctrl+J insert a newline. Terminals
// cannot report plain Shift+Enter, so the promise moves to these keys.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message, or /help. ⌥Enter for newline"
	ta.Prompt = "❯ "
	ta.FocusedStyle.Prompt = styleUser
	ta.FocusedStyle.Text = styleAssistant
	ta.FocusedStyle.Placeholder = styleHints
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.Style = styleUser
	ta.ShowLineNumbers = false
	ta.CharLimit = 100000
	ta.MaxHeight = maxInputLines
	ta.SetHeight(1)
	ta.Focus()
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("alt+enter", "ctrl+j"),
		key.WithHelp("alt+enter", "insert newline"),
	)
	return ta
}

// NewWithResume builds the TUI model and optionally opens the resume picker.
func NewWithResume(ag *agent.Agent, autoResume bool) Model {
	var client protocol.SessionClient
	if ag != nil {
		client = ag
	}
	workspace := ""
	if ag != nil {
		workspace = ag.WorkspaceLabel()
	}
	m := NewWithClient(client, workspace, autoResume)
	m.ag = ag
	if ag != nil {
		ag.SetChildInteraction(true)
		m.installSessionRouting(ag.Sessions())
	}
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	return m
}

// NewWithClient creates a TUI over the narrow application protocol. It is
// useful for tests and for future app/session adapters that do not expose an
// *agent.Agent. No command or event channel is needed by this constructor.
func NewWithClient(client protocol.SessionClient, workspace string, autoResume bool) Model {
	ta := newTextarea()

	vp := viewport.New(80, 20)
	vp.Style = lipgloss.NewStyle().Padding(0, 1)

	sp := spinner.New()
	sp.Style = styleRunning
	sp.Spinner = spinner.Spinner{Frames: []string{"·", "✧", "✦", "✧"}, FPS: time.Second / 6}

	m := Model{
		client:       client,
		viewport:     vp,
		textarea:     ta,
		spinner:      sp,
		workspace:    workspace,
		autoResume:   autoResume,
		followOutput: true,
		inlineMode:   true,
		statusItems:  append([]string(nil), defaultStatusItems...),
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
	if client != nil && !autoResume && (!m.hasSnapshot || len(m.snapshot.History) == 0) {
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
	// Bubble Tea owns terminal capability negotiation. We intentionally do not
	// enable mouse tracking or alternate-scroll in the default ordinary screen;
	// native selection/scrollback and Cmd-C remain terminal operations.
	cmds := []tea.Cmd{m.openWatchCmd(), m.spinner.Tick, noticeTickCmd(), tea.SetWindowTitle("ccdp — " + m.workspace)}
	if m.routing != nil {
		cmds = append(cmds, m.loadAgentCatalog(false), agentCatalogTickCmd(m.routing.directory))
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
	case effortTickMsg:
		return m, m.advanceEffortMotion(msg)
	case clipboardReadMsg:
		m.routeClipboardRead(msg)
		return m, nil
	case agentCatalogTick:
		if m.routing == nil || msg.directory != m.routing.directory {
			return m, nil
		}
		return m, tea.Batch(m.loadAgentCatalog(false), agentCatalogTickCmd(msg.directory))
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
			return m, nil
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
		return m, tea.Batch(m.openWatchCmd(), m.flushInline())
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
		return m, tea.Batch(modalMouseTransition(wasModal, &m), boolCmd(releaseMouse, disableMouseCmd()))

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
		return m, boolCmd(releaseMouse, disableMouseCmd())

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
		} else {
			m.pushStatus("copied latest response")
		}
		return m, nil

	case resumeOpenedMsg:
		wasModal := m.question != nil || m.approval != nil || m.picker != nil
		model, cmd := m.handleResumeOpened(msg)
		return model, tea.Batch(cmd, modalMouseTransition(wasModal, model))

	case resumeCandidateClosedMsg:
		return m, nil

	case spinner.TickMsg:
		// Spinner ticks continue while the TUI is idle, so they also provide a
		// bounded expiry path for a notice added after the dedicated notice tick
		// stopped at an empty queue.
		m.expireNotices(now())
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case errMsg:
		m.pushStatus("error: " + msg.err.Error())
		return m, nil

	case initResumeMsg:
		m.autoResume = false // only once
		return m, m.startResumePicker()

	}

	// Default: forward to textarea (keeps cursor behavior live).
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// layout recomputes viewport/input sizes after a resize.
func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.textarea.SetWidth(max(1, m.width-2))
	m.textarea.SetHeight(m.desiredInputHeight())
	// Keep the no-popup baseline in terms of the same wrapped presentation
	// segments View uses. A fixed one-row guess causes the transcript anchor to
	// drift when the status/footer or composer wraps on a narrow terminal.
	m.baseVpH = max(1, m.height-m.fixedPresentationHeight())
	m.syncViewportHeight()
	m.viewport.Width = m.width
	m.render()
	if m.followOutput {
		m.viewport.GotoBottom()
	}
}

const maxInputLines = 5
const maxPickerRows = 10

// desiredInputHeight grows the composer for explicit newlines and wrapped
// text, while keeping an empty/single-line prompt compact.
func (m *Model) desiredInputHeight() int {
	if m.pasteFold != nil && !m.pasteFold.Expanded {
		return 1
	}
	width := m.textarea.Width()
	if width < 1 {
		width = 1
	}
	rows := 0
	for _, line := range strings.Split(m.textarea.Value(), "\n") {
		lineWidth := lipgloss.Width(line)
		rows += max(1, (lineWidth+width-1)/width)
	}
	return min(maxInputLines, max(1, rows))
}

func (m *Model) syncInputHeight() {
	desired := m.desiredInputHeight()
	if desired == m.textarea.Height() {
		return
	}
	m.textarea.SetHeight(desired)
	if m.height > 0 {
		m.baseVpH = max(1, m.height-m.fixedPresentationHeight())
		m.syncViewportHeight()
	}
	if m.followOutput {
		m.viewport.GotoBottom()
	}
}

// fixedPresentationHeight measures the chrome that is present even when no
// popup is open. Optional decision, selector, and command-suggestion surfaces
// are measured separately so baseVpH remains a stable no-popup anchor.
func (m *Model) fixedPresentationHeight() int {
	if m == nil {
		return 0
	}
	width := m.width
	if width <= 0 {
		width = m.viewport.Width
	}
	width = max(1, width)
	total := 0
	footer := m.renderFooter()
	if !m.hasSnapshot {
		// View prepends this state line during the initial snapshot handoff.
		// Include it here so the viewport uses the same budget before and after
		// the first authoritative snapshot arrives.
		if state := m.renderPersistentStatus(); state != "" {
			footer = state + "\n" + footer
		}
	}
	for _, text := range []string{m.headerPresentation(), m.renderStatus(), m.renderInput(), footer} {
		total += presentationHeight(text, width)
	}
	return total
}

// headerPresentation is shared by View and the viewport row budget.
// A single source lets state changes (active child count,
// target selection, or a narrow route label) update the viewport immediately,
// without waiting for a terminal resize.
func (m *Model) headerPresentation() string {
	if m == nil {
		return ""
	}
	header := m.renderHeader()
	if m.routing == nil {
		return header
	}
	label := "main"
	var active, approvals int
	for _, row := range m.routing.rows {
		if row.Run.Active() {
			active++
		}
		if row.Approval != nil {
			approvals++
		}
		if string(row.SessionID) == m.sessionID {
			label = fmt.Sprintf("main › %s · %s", strings.Join(strings.Fields(row.Title), " "), row.Run.Status)
		}
	}
	if m.sessionID == m.routing.rootID {
		if active > 0 {
			label += fmt.Sprintf(" · %d active", active)
		}
		if approvals > 0 {
			label += fmt.Sprintf(" · %d approvals", approvals)
		}
		if active > 0 || approvals > 0 {
			label += " · /agents"
		}
	}
	if m.sessionID == m.routing.rootID && active == 0 && approvals == 0 {
		return header
	}
	routeText := "target=" + sanitizeANSI(m.sessionID)
	if m.width >= 34 {
		routeText += " · " + sanitizeANSI(label)
	}
	route := truncateDisplay(routeText, m.width)
	if header == "" {
		return route
	}
	return header + "\n" + route
}

func (m *Model) optionalPresentationHeight() int {
	if m == nil {
		return 0
	}
	width := m.width
	if width <= 0 {
		width = m.viewport.Width
	}
	width = max(1, width)
	total := 0
	for _, text := range []string{m.renderInlineDecision(), m.renderInlineSurface(), m.renderCmdSuggest()} {
		total += presentationHeight(text, width)
	}
	return total
}

// syncViewportHeight reserves exactly the rows occupied by the currently
// rendered decision/selector/suggestion surfaces. The measurements use the
// presentation helpers, so wrapped descriptions and multi-line hints consume
// their real terminal rows instead of a guessed popup constant.
func (m *Model) syncViewportHeight() {
	if m.height > 0 {
		// Status, routing, and footer rows can change without a WindowSizeMsg.
		// Refresh the baseline on every sync so a child target or a wrapped
		// notice never steals the first transcript row.
		m.baseVpH = max(1, m.height-m.fixedPresentationHeight())
	}
	h := m.baseVpH
	if h == 0 {
		if m.height > 0 {
			h = max(1, m.height-m.fixedPresentationHeight())
		} else {
			h = 20 // sane default before the first WindowSizeMsg
		}
	}
	if m.height <= 0 {
		// Before the first WindowSizeMsg the renderer has no terminal width or
		// height to measure against. Keep the constructor/test fallback usable;
		// real frames take the measured path below as soon as the window size is
		// known.
		if n := len(m.cmdSug); n > 0 {
			n = min(n, m.cmdSuggestionPageSize())
			if len(m.cmdSug) > n {
				n++
			}
			h -= n + 3
		}
		if m.picker != nil && m.picker.inline {
			h -= m.pickerVisibleRows() + 3
		}
	} else {
		h -= m.optionalPresentationHeight()
	}
	if h < 1 {
		h = 1
	}
	m.viewport.Height = h
}

// handleKey routes key presses based on the current state.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The detail reader owns the complete key stream while open. In particular,
	// arrows and Enter must never reach the composer or submit a command.
	if m.reader != nil {
		return m.handleReaderKey(msg)
	}
	if msg.String() == "ctrl+o" && m.routing != nil {
		return m, m.loadAgentCatalog(true)
	}
	// Modal components get first refusal through the shared focus router. This
	// keeps selector/approval keys out of the composer and gives every menu the
	// same lifecycle boundary.
	if model, cmd, handled := m.focus.Route(m, msg); handled {
		return model, cmd
	}
	// A terminal paste arrives as a KeyRunes message with Paste=true. Insert it
	// literally so a pasted slash command cannot execute until the user presses
	// Enter explicitly. The clipboard-read path below handles image/mixed paste
	// messages that do not carry runes.
	if msg.Paste && len(msg.Runes) > 0 {
		m.quitArmed = false
		text := string(msg.Runes)
		m.textarea.InsertString(text)
		m.recordPastedText(text)
		m.syncInputHeight()
		m.refreshCmdSuggest()
		return m, nil
	}
	if msg.Type == tea.KeyCtrlV || msg.String() == "shift+insert" || (msg.Paste && len(msg.Runes) == 0) {
		m.quitArmed = false
		return m, m.pasteClipboard()
	}
	if len(m.inputImages) > 0 {
		switch msg.String() {
		case "alt+backspace":
			m.removeInputImage()
			return m, nil
		case "alt+left":
			m.selectedImage = (m.selectedImage + len(m.inputImages) - 1) % len(m.inputImages)
			return m, nil
		case "alt+right":
			m.selectedImage = (m.selectedImage + 1) % len(m.inputImages)
			return m, nil
		}
	}
	if msg.String() == "ctrl+e" && m.togglePasteFold() {
		m.quitArmed = false
		m.syncInputHeight()
		return m, nil
	}

	// Slash-command autocomplete popup: up/down navigate, Tab completes the
	// highlighted command for further editing, and Enter executes it.
	if len(m.cmdSug) > 0 {
		switch msg.String() {
		case "up":
			m.quitArmed = false
			m.cmdSugIdx--
			if m.cmdSugIdx < 0 {
				m.cmdSugIdx = len(m.cmdSug) - 1
			}
			return m, nil
		case "down":
			m.quitArmed = false
			m.cmdSugIdx = (m.cmdSugIdx + 1) % len(m.cmdSug)
			return m, nil
		case "pgup", "ctrl+u":
			m.quitArmed = false
			m.cmdSugIdx = max(0, m.cmdSugIdx-m.cmdSuggestionPageSize())
			return m, nil
		case "pgdown", "ctrl+d":
			m.quitArmed = false
			m.cmdSugIdx = min(len(m.cmdSug)-1, m.cmdSugIdx+m.cmdSuggestionPageSize())
			return m, nil
		case "home":
			m.quitArmed = false
			m.cmdSugIdx = 0
			return m, nil
		case "end":
			m.quitArmed = false
			m.cmdSugIdx = len(m.cmdSug) - 1
			return m, nil
		case "tab":
			return m.acceptCmdSuggestion(), nil
		case "enter":
			// Enter executes the highlighted command in one step; Tab only
			// completes it so arguments can be added.
			m.acceptCmdSuggestion()
			return m.submit()
		case "esc":
			m.quitArmed = false
			m.closeCmdSuggest()
			return m, nil
		}
	}

	switch msg.String() {
	case "ctrl+c":
		if m.busy {
			m.quitArmed = false
			m.interruptRequested = true
			m.pushStatus("interrupting agent…")
			return m, m.submitCommand(protocol.Command{Type: protocol.CommandInterrupt}, "interrupt requested")
		}
		// Two-stage exit (Claude Code): the first press asks for
		// confirmation, the second actually quits.
		if m.quitArmed {
			return m, tea.Quit
		}
		m.quitArmed = true
		m.pushStatus("press ctrl+c again to quit")
		return m, nil

	case "pgup", "ctrl+u":
		m.quitArmed = false
		m.viewport.HalfPageUp()
		m.followOutput = false
		return m, nil
	case "pgdown", "ctrl+d":
		m.quitArmed = false
		m.viewport.HalfPageDown()
		m.followOutput = m.viewport.AtBottom()
		return m, nil
	case "home":
		m.quitArmed = false
		m.expandPasteFoldForEdit()
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.refreshPasteFold()
		m.syncInputHeight()
		return m, cmd
	case "end":
		m.quitArmed = false
		m.expandPasteFoldForEdit()
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.refreshPasteFold()
		m.syncInputHeight()
		return m, cmd
	case "up":
		// A single-line composer uses arrows for input history. Multi-line input
		// keeps normal textarea cursor motion; PgUp/PgDn scroll the transcript.
		if m.textarea.LineCount() <= 1 {
			if len(m.history) > 0 {
				return m.historyPrev(), nil
			}
			m.viewport.LineUp(3)
			m.followOutput = false
			return m, nil
		}
		if len(m.history) > 0 && m.textarea.Line() == 0 && m.textarea.LineInfo().RowOffset == 0 {
			return m.historyPrev(), nil
		}
	case "down":
		if m.textarea.LineCount() <= 1 {
			if len(m.history) > 0 {
				return m.historyNext(), nil
			}
			m.viewport.LineDown(3)
			m.followOutput = m.viewport.AtBottom()
			return m, nil
		}
		if len(m.history) > 0 && m.textarea.Line() == m.textarea.LineCount()-1 {
			info := m.textarea.LineInfo()
			if info.RowOffset+1 >= info.Height {
				return m.historyNext(), nil
			}
		}

	case "ctrl+p":
		return m.historyPrev(), nil
	case "ctrl+n":
		return m.historyNext(), nil

	case "enter":
		return m.submit()

	case "esc":
		m.quitArmed = false
		if m.routing != nil && m.sessionID != m.routing.rootID {
			return m, m.openAgentView(m.parentAgentID())
		}
		m.closeCmdSuggest()
		// Esc dismisses temporary UI while preserving the root draft and its
		// attachments. A pending clipboard request is cancelled by invalidating
		// its token, so a late host result cannot resurrect an attachment.
		m.pasteRequest = ""
		return m, nil
	}

	// Normal typing.
	m.quitArmed = false
	m.expandPasteFoldForEdit()
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refreshPasteFold()
	m.syncInputHeight()
	m.refreshCmdSuggest()
	return m, cmd
}

// maxCmdSuggestions caps only the visible command window. All matches remain
// selectable with up/down, page keys, home and end.
const maxCmdSuggestions = 8

func (m *Model) cmdSuggestionPageSize() int {
	if m.height <= 0 {
		return maxCmdSuggestions
	}
	// Reserve rows for header, transcript, status, popup border, composer and
	// footer. Small terminals still retain one selectable candidate.
	return min(maxCmdSuggestions, max(1, m.height-8))
}

// refreshCmdSuggest recomputes the slash-command popup from the current input:
// it opens whenever the input is a "/"-prefixed command name (no whitespace
// yet) that is a strict prefix of at least one command, and closes otherwise.
func (m *Model) refreshCmdSuggest() {
	m.closeCmdSuggest()
	val := m.textarea.Value()
	if len(val) < 1 || val[0] != '/' || strings.ContainsAny(val, " \t") {
		return
	}
	prefix := strings.TrimPrefix(val, "/")
	matches := commandCatalog.Suggestions(prefix)
	for _, name := range m.customCommandNames() {
		if strings.HasPrefix(name, prefix) && !containsString(matches, name) {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	if len(matches) == 0 || (len(matches) == 1 && matches[0] == prefix) {
		return
	}
	m.cmdSug = matches
	m.cmdSugIdx = 0
	m.syncViewportHeight()
}

// acceptCmdSuggestion fills the highlighted command into the input box.
func (m *Model) acceptCmdSuggestion() tea.Model {
	if len(m.cmdSug) == 0 {
		return m
	}
	m.textarea.SetValue("/" + m.cmdSug[m.cmdSugIdx])
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.closeCmdSuggest()
	return m
}

func (m *Model) closeCmdSuggest() {
	m.cmdSug = nil
	m.cmdSugIdx = 0
	m.syncViewportHeight()
}

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

// handleApprovalKey resolves an approval modal.
func (m *Model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.approval == nil {
		return m, nil
	}
	if msg.String() == "esc" && m.routing != nil && m.sessionID != m.routing.rootID {
		return m, m.openAgentView(m.parentAgentID())
	}
	// Keep long approval details scrollable while a decision is in flight, but
	// never submit a second decision for the same modal before its receipt.
	if m.approvalPending {
		switch msg.String() {
		case "y", "Y", "enter", "n", "N", "esc", "a", "A", "x", "X":
			return m, nil
		}
	}
	var approve, remember bool
	handled := true
	switch msg.String() {
	case "up", "k":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.Offset = m.approvalScroll
		m.approvalState.Move(-1)
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "down", "j":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.Offset = m.approvalScroll
		m.approvalState.Move(1)
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "pgup", "ctrl+u":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.Offset = m.approvalScroll
		m.approvalState.PageUp()
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "pgdown", "ctrl+d":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.Offset = m.approvalScroll
		m.approvalState.PageDown()
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "home":
		m.approvalState.Home()
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "end":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.End()
		m.approvalScroll = m.approvalState.Offset
		return m, nil
	case "y", "Y", "enter":
		approve, remember = true, false
	case "n", "N", "esc":
		approve, remember = false, false
	case "a", "A":
		approve, remember = true, true
	case "x", "X":
		approve, remember = false, true
	default:
		handled = false
	}
	if handled {
		approval := *m.approval
		if approval.Tool == "Plan" {
			cmd := protocol.Command{Type: protocol.CommandApprovePlan,
				Plan: &protocol.ApprovePlan{PlanID: approval.ID, Approve: approve}}
			m.approvalPending = true
			m.pendingApprovalCommand = cmd.ID
			if cmd.ID == "" {
				cmd.ID = nextUICommandID()
				m.pendingApprovalCommand = cmd.ID
			}
			return m, m.submitCommand(cmd, "plan decision submitted")
		}
		cmd := protocol.Command{Type: protocol.CommandApproveTool,
			Approval: &protocol.ApproveTool{ApprovalID: approval.ID, Approve: approve, Remember: remember}}
		m.approvalPending = true
		m.pendingApprovalCommand = cmd.ID
		if cmd.ID == "" {
			cmd.ID = nextUICommandID()
			m.pendingApprovalCommand = cmd.ID
		}
		return m, m.submitCommand(cmd, "approval decision submitted")
	}
	return m, nil
}

// handlePickerKey resolves the interactive list picker.
func (m *Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picker == nil {
		return m, nil
	}
	if m.picker.action.Kind == selectorEffort && msg.String() != "enter" && msg.String() != "esc" && msg.String() != "ctrl+c" {
		return m, m.adjustEffort(msg)
	}
	switch msg.String() {
	case "up", "k", "fn+up", "alt+up", "shift+up", "mousewheelup", "wheelup":
		m.picker.buf = ""
		step := 1
		if strings.HasPrefix(msg.String(), "fn+") || strings.HasPrefix(msg.String(), "alt+") || strings.HasPrefix(msg.String(), "shift+") {
			step = maxPickerRows
		}
		m.picker.index -= step
		if m.picker.index < 0 {
			m.picker.index = 0
		}
	case "down", "j", "fn+down", "alt+down", "shift+down", "mousewheeldown", "wheeldown":
		m.picker.buf = ""
		step := 1
		if strings.HasPrefix(msg.String(), "fn+") || strings.HasPrefix(msg.String(), "alt+") || strings.HasPrefix(msg.String(), "shift+") {
			step = maxPickerRows
		}
		m.picker.index = min(len(m.picker.lines)-1, m.picker.index+step)
	case "pgup", "ctrl+u":
		m.picker.buf = ""
		m.picker.index = max(0, m.picker.index-maxPickerRows)
	case "pgdown", "ctrl+d":
		m.picker.buf = ""
		m.picker.index = min(len(m.picker.lines)-1, m.picker.index+maxPickerRows)
	case "home":
		m.picker.buf = ""
		m.picker.index = 0
	case "end":
		m.picker.buf = ""
		m.picker.index = len(m.picker.lines) - 1
	case "enter":
		selection := m.picker.index
		if m.picker.buf != "" {
			n := 0
			for _, r := range m.picker.buf {
				n = n*10 + int(r-'0')
			}
			selection = n - 1
		}
		if selection < 0 || selection >= len(m.picker.lines) {
			m.pushStatus("picker: enter a number between 1 and " + strconv.Itoa(len(m.picker.lines)))
			m.picker.buf = ""
			return m, nil
		}
		// A disabled option is an explanation surface, not an executable
		// action. Keep the picker open so the user can read its reason, move to
		// an available option, or cancel without accidentally submitting a
		// mutation.
		if selection < len(m.picker.options) && m.picker.options[selection].Disabled {
			reason := strings.TrimSpace(m.picker.options[selection].Description)
			if reason == "" {
				reason = "option unavailable"
			}
			m.pushStatus(reason)
			m.picker.buf = ""
			m.syncPickerSelector()
			return m, nil
		}
		p := *m.picker
		m.picker = nil
		m.syncViewportHeight()
		if selection < len(p.options) {
			m.mouseReleasePending = true
			cmd := m.executeSelectorAction(p.action, p.options[selection].ID)
			if cmd == nil {
				m.mouseReleasePending = false
				return m, disableMouseCmd()
			}
			return m, cmd
		}
		return m, disableMouseCmd()
	case "esc", "ctrl+c":
		m.pushStatus("picker cancelled")
		m.picker = nil
		m.syncViewportHeight()
		return m, disableMouseCmd()
	default:
		if msg.Type == tea.KeyRunes {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, msg.String())
			if digits != "" && len(m.picker.buf) < len(strconv.Itoa(len(m.picker.lines))) {
				m.picker.buf += digits
				if n, err := strconv.Atoi(m.picker.buf); err == nil && n >= 1 && n <= len(m.picker.lines) {
					m.picker.index = n - 1
				}
				m.pushStatus("pick 1–" + strconv.Itoa(len(m.picker.lines)) + " (enter confirms, esc cancels): " + m.picker.buf)
			}
		}
	}
	m.syncPickerSelector()
	return m, nil
}

func disableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.DisableMouse() }
}

func enableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.EnableMouseCellMotion() }
}

func modalMouseTransition(wasModal bool, model tea.Model) tea.Cmd {
	current, ok := model.(Model)
	if !ok {
		if pointer, pointerOK := model.(*Model); pointerOK && pointer != nil {
			current = *pointer
			ok = true
		}
	}
	if !ok {
		return nil
	}
	isModal := current.question != nil || current.approval != nil || current.picker != nil
	if !wasModal && isModal {
		return enableMouseCmd()
	}
	if wasModal && !isModal {
		return disableMouseCmd()
	}
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

func (m *Model) syncPickerSelector() {
	if m == nil || m.picker == nil {
		return
	}
	m.picker.selector.Index = m.picker.index
	m.picker.selector.SetSize(m.pickerVisibleRows())
	// Keep the selected index synchronized with the shared selector and its
	// scroll state.
	m.picker.index = m.picker.selector.Index
}

func (m *Model) startSelectorAt(title string, options []selectorOption, selected int, inline bool, action selectorAction) tea.Cmd {
	lines := make([]string, len(options))
	for i, option := range options {
		lines[i] = option.Label
	}
	if len(lines) == 0 {
		m.pushStatus(title + ": no items")
		return nil
	}
	selected = min(max(0, selected), len(lines)-1)
	selectorOptions := make([]SelectorOption, len(options))
	copy(selectorOptions, options)
	selector := Selector{Title: title, Options: selectorOptions, Index: selected}
	selector.SetSize(m.pickerVisibleRows())
	m.picker = &pickerState{title: title, lines: lines, options: options, selector: selector,
		action: action, index: selected, inline: inline}
	m.picker.selector.SetSize(m.pickerVisibleRows())
	m.syncViewportHeight()
	if !inline {
		m.pushStatus(title + " (↑/↓ navigate, enter confirms, esc cancels)")
	}
	return enableMouseCmd()
}

// submit handles the Enter key in the input area.
func (m *Model) submit() (tea.Model, tea.Cmd) {
	raw := m.textarea.Value()
	if m.missingHistoryImages && !strings.HasPrefix(strings.TrimSpace(raw), "/") {
		m.pushStatus("older image attachments expired from input history; paste them again or clear the draft")
		return m, nil
	}
	if m.pasteRequest != "" {
		m.pushStatus("reading clipboard; wait before sending")
		return m, nil
	}
	if strings.TrimSpace(raw) == "" && len(m.inputImages) == 0 {
		return m, nil
	}
	if m.resumePending {
		m.pushStatus("resume is opening; please wait")
		return m, nil
	}
	text := raw
	if strings.HasPrefix(strings.TrimSpace(raw), "/") {
		text = strings.TrimSpace(raw)
	}
	m.textarea.Reset()
	m.pasteFold = nil
	m.syncInputHeight()
	m.closeCmdSuggest()
	m.history = append(m.history, text)
	m.historyIdx = len(m.history)
	m.followOutput = true
	// The line above the composer is ephemeral: every new submission either
	// replaces it with fresh status or clears it before producing durable output.
	m.status = ""

	if strings.HasPrefix(text, "/") {
		// Keep query/report commands in the transcript, but treat selectors and
		// actions like /model and /mode as composer-adjacent transient state.
		if slashCommandHasTranscriptOutput(text) {
			m.pushLog("command", text)
		}
		return m.runCommand(text)
	}
	images := cloneInputImages(m.inputImages)
	m.rememberImageHistory(len(m.history)-1, images)
	m.inputImages = nil
	m.layout()

	m.viewport.GotoBottom()
	commandID := nextUICommandID()
	if m.retryCommandID != "" && m.retryDraft == text && sameInputImages(m.retryImages, images) {
		// A rejected/transport-failed input is the same user intent when the
		// restored draft is submitted again; reuse its durable command id so a
		// runtime dedupe cannot create a second message.
		commandID = m.retryCommandID
		m.retryCommandID = ""
		m.retryDraft = ""
		m.retryImages = nil
		if m.receiptKeys != nil {
			delete(m.receiptKeys, commandID)
			for i, id := range m.receiptOrder {
				if id == commandID {
					m.receiptOrder = append(m.receiptOrder[:i], m.receiptOrder[i+1:]...)
					break
				}
			}
		}
	}
	m.rememberSubmission(commandID, text)
	if len(images) > 0 {
		if m.pendingSubmissions == nil {
			m.pendingSubmissions = make(map[protocol.CommandID]string)
		}
		m.pendingSubmissions[commandID] = text
		if m.pendingImages == nil {
			m.pendingImages = make(map[protocol.CommandID][]protocol.InputImage)
		}
		m.pendingImages[commandID] = images
	}
	cmd := protocol.NewSubmitInput(commandID, protocol.SessionID(m.sessionID), protocol.InputID(commandID), text, protocol.InputSteer)
	cmd.Input.Images = images
	return m, m.submitCommand(cmd, "message submitted")
}

// slashCommandHasTranscriptOutput separates durable command results from
// selectors/actions whose feedback belongs in the replaceable status line.
func slashCommandHasTranscriptOutput(text string) bool {
	fields, err := commands.ParseLine(text)
	if err != nil {
		return true
	}
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]
	return commandCatalog.ShouldTranscript(cmd, args)
}

func (m *Model) historyPrev() tea.Model {
	if len(m.history) == 0 {
		return m
	}
	if m.historyIdx <= 0 {
		m.historyIdx = 1
	}
	m.historyIdx--
	if m.historyIdx < 0 {
		m.historyIdx = 0
	}
	m.pasteFold = nil
	m.textarea.SetValue(m.history[m.historyIdx])
	m.recallHistoryImages()
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.refreshCmdSuggest()
	return m
}

func (m *Model) historyNext() tea.Model {
	if len(m.history) == 0 {
		return m
	}
	if m.historyIdx >= len(m.history)-1 {
		m.historyIdx = len(m.history)
		m.pasteFold = nil
		m.textarea.SetValue("")
		m.recallHistoryImages()
		m.syncInputHeight()
		m.refreshCmdSuggest()
		return m
	}
	m.historyIdx++
	m.pasteFold = nil
	m.textarea.SetValue(m.history[m.historyIdx])
	m.recallHistoryImages()
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.refreshCmdSuggest()
	return m
}

func (m *Model) pushStatus(s string) {
	m.status = s
}

func (m *Model) setStatus(format string, args ...any) {
	m.status = fmt.Sprintf(format, args...)
}
