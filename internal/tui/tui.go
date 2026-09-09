// Package tui implements the terminal user interface for ccdp. It follows
// Claude Code's interactive chat model (streaming assistant text, permission
// modals, slash commands) rendered with Bubble Tea.
package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/agent"
	"ccdp/internal/permissions"
)

const appVersion = "0.2.0"

// Colors and styles.
var (
	styleHeader    = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Bold(true).Padding(0, 1)
	styleUser      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleAssistant = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	styleSystem    = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	styleToolRun   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleToolOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleToolErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleStatus    = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	styleError     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleInput     = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	styleFooter    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleHints     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleDivider   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleModal     = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("214")).
			Padding(1, 2).
			Width(56)
	styleModalTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleModeBadge   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229"))
	styleRunning     = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true)
	styleCmdSug      = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	styleCmdSugSel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("62"))
	styleContextOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleContextWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
)

// logItem is one rendered entry in the conversation viewport.
type logItem struct {
	kind   string // user | assistant | system | tool | status | error | thinking
	text   string
	toolID string
	status string // running | success | error | denied

	// sanitizeANSI display cache (valid while sanitizedLen == len(text)).
	sanitized    string
	sanitizedLen int
}

// Model is the root Bubble Tea model.
type Model struct {
	width, height int

	viewport viewport.Model
	textarea textarea.Model
	spinner  spinner.Model

	ag     *agent.Agent
	events <-chan agent.Event
	ctrl   chan<- agent.Control

	items     []logItem // conversation log
	streaming bool      // an assistant message is being streamed
	busy      bool
	status    string

	approval *agent.ApprovalRequest

	modelName string
	mode      permissions.Mode
	planMode  bool
	sessionID string
	workspace string

	usage agent.Usage // token/cost accounting, updated on EventUsage

	history    []string
	historyIdx int

	// picker drives the interactive list picker (e.g. /rewind). While active
	// the next input is a line number; esc cancels.
	picker *pickerState

	// cmdSug is the live slash-command autocomplete list (popup above the
	// input) while the user is typing a command name after "/". cmdSugIdx is
	// the highlighted entry; up/down navigate, tab/enter accept.
	cmdSug    []string
	cmdSugIdx int

	// customCmds holds user-defined slash commands loaded from
	// ~/.ccdp/commands and <workspace>/.ccdp/commands.
	customCmds []customCommand

	modalErr string // transient message inside the modal area
	quit     bool

	// turnStarted is when the current turn began, for the completion bell.
	turnStarted time.Time

	// quitArmed latches the first ctrl+c press when idle: the second press
	// quits (Claude Code's two-stage exit), any other key disarms it.
	quitArmed bool

	// baseVpH is the transcript viewport height with no popup open; the
	// command-suggestion popup shrinks the viewport so the total screen
	// height stays constant.
	baseVpH int

	// statusItems configures which items render in the header statusline
	// (/statusline). Valid tokens: version model mode session workspace cost.
	statusItems []string

	// autoResume opens the session picker on first render (ccdp -c).
	autoResume bool
}

// pickerState is a lightweight numbered-list selector.
type pickerState struct {
	title  string
	lines  []string
	onPick func(selection int) // selection = chosen index (0-based)
	buf    string              // digits typed so far
}

// defaultStatusItems is what the header shows unless /statusline changes it.
var defaultStatusItems = []string{"version", "model", "mode", "session", "workspace", "cost", "context"}

// eventMsg wraps an agent event for the tea runtime.
type eventMsg struct{ ev agent.Event }

// errMsg is a local error surfaced to the UI.
type errMsg struct{ err error }

// New builds the TUI model. With autoResume set (ccdp -c) it immediately opens
// the saved-session picker.
func New(ag *agent.Agent) Model {
	return NewWithResume(ag, false)
}

// newTextarea builds the input textarea with ccdp's key bindings: Enter
// submits (handled by handleKey), Alt+Enter / Ctrl+J insert a newline. Terminals
// cannot report plain Shift+Enter, so the promise moves to these keys.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message, or /help. Alt+Enter for newline"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 100000
	ta.SetHeight(2)
	ta.Focus()
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("alt+enter", "ctrl+j"),
		key.WithHelp("alt+enter", "insert newline"),
	)
	return ta
}

// NewWithResume builds the TUI model and optionally opens the resume picker.
func NewWithResume(ag *agent.Agent, autoResume bool) Model {
	ta := newTextarea()

	vp := viewport.New(80, 20)
	vp.Style = lipgloss.NewStyle().Padding(0, 1)

	sp := spinner.New()
	sp.Style = styleRunning
	sp.Spinner = spinner.Dot

	m := Model{
		ag:         ag,
		events:     ag.Events(),
		ctrl:       ag.Controls(),
		viewport:   vp,
		textarea:   ta,
		spinner:    sp,
		modelName:  ag.Model(),
		mode:       ag.PermissionMode(),
		sessionID:  ag.SessionID(),
		workspace:  ag.WorkspaceLabel(),
		autoResume: autoResume,
		customCmds: loadCustomCommands(ag.WorkspaceLabel()),
	}

	// Greet the user on a fresh session (no agent turn is triggered).
	if !ag.HasHistory() && !autoResume {
		m.items = append(m.items, logItem{kind: "system", text: welcomeBanner(ag.WorkspaceLabel())})
	}
	return m
}

func welcomeBanner(ws string) string {
	return fmt.Sprintf(`Hello! I'm ccdp, running in %s.

I can read, write, edit and explore your code, run shell commands, plan with
todos, and verify my work. Start with a request like:
  - "explain what this repo does"
  - "find the bug in <file> and fix it"
  - "add a test for <function>"

Type /help for commands. Dangerous actions will ask for your approval.`, ws)
}

// Init starts the event watcher and spinner, and titles the terminal window.
func (m Model) Init() tea.Cmd {
	// OSC 0: set window title (workspace in the title helps multitaskers).
	fmt.Fprintf(os.Stdout, "\x1b]0;ccdp — %s\x07", m.workspace)
	cmds := []tea.Cmd{m.waitEvent(), m.spinner.Tick}
	if m.autoResume {
		m.autoResume = false // only once
		m.startResumePicker()
	}
	return tea.Batch(cmds...)
}

// waitEvent produces a Cmd that receives the next agent event.
func (m Model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		return eventMsg{ev: <-m.events}
	}
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case eventMsg:
		return m.handleEvent(msg.ev)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case errMsg:
		m.pushStatus("error: " + msg.err.Error())
		return m, nil
	}

	// Default: forward to textarea (keeps cursor behavior live).
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// layout recomputes viewport/input sizes after a resize.
func (m *Model) layout() {
	if m.width < 20 || m.height < 8 {
		return
	}
	headerH := 1
	statusH := 1
	footerH := 1
	inputH := 3
	vpH := m.height - headerH - statusH - inputH - footerH
	if vpH < 3 {
		vpH = 3
	}
	m.baseVpH = vpH
	m.syncViewportHeight()
	m.viewport.Width = m.width
	m.textarea.SetWidth(m.width - 2)
	m.render()
	m.viewport.GotoBottom()
}

// syncViewportHeight reserves room for the command-suggestion popup so opening
// it does not push the footer off-screen.
func (m *Model) syncViewportHeight() {
	h := m.baseVpH
	if h == 0 {
		h = 20 // sane default before the first WindowSizeMsg
	}
	if n := len(m.cmdSug); n > 0 {
		h -= n + 3 // content lines + border rows + spacing line
		if h < 3 {
			h = 3
		}
	}
	m.viewport.Height = h
}

// handleEvent applies an agent event to the UI state.
func (m *Model) handleEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	switch ev.Type {
	case agent.EventUserMsg:
		m.items = append(m.items, logItem{kind: "user", text: ev.Text})
		m.busy = true
		m.streaming = false
		m.turnStarted = time.Now()

	case agent.EventStream:
		if !m.streaming {
			m.items = append(m.items, logItem{kind: "assistant", text: ""})
			m.streaming = true
		}
		i := len(m.items) - 1
		m.items[i].text += ev.Text

	case agent.EventReasoning:
		// Chain-of-thought: fold into the leading "thinking" entry, or open one.
		found := false
		for i := len(m.items) - 1; i >= 0; i-- {
			if m.items[i].kind == "thinking" {
				m.items[i].text += ev.Text
				found = true
				break
			}
			if m.items[i].kind == "assistant" || m.items[i].kind == "user" || m.items[i].kind == "tool" {
				break
			}
		}
		if !found {
			m.items = append(m.items, logItem{kind: "thinking", text: ev.Text})
		}

	case agent.EventToolResult:
		t := ev.Tool
		if t == nil {
			break
		}
		// Merge with an existing running entry for the same tool id.
		found := false
		for i := len(m.items) - 1; i >= 0; i-- {
			if m.items[i].kind == "tool" && m.items[i].toolID == t.ID {
				m.items[i].status = t.Status
				m.items[i].text = renderToolText(t.Name, t.Args, t.Status, t.Output)
				found = true
				break
			}
		}
		if !found {
			m.items = append(m.items, logItem{
				kind: "tool", toolID: t.ID, status: t.Status,
				text: renderToolText(t.Name, t.Args, t.Status, t.Output),
			})
		}

	case agent.EventToolStart:
		t := ev.Tool
		if t != nil {
			m.items = append(m.items, logItem{
				kind: "tool", toolID: t.ID, status: "running",
				text: renderToolText(t.Name, t.Args, "running", ""),
			})
		}

	case agent.EventToolStream:
		t := ev.Tool
		if t == nil {
			break
		}
		// Append the line to the running tool entry (live output).
		for i := len(m.items) - 1; i >= 0; i-- {
			if m.items[i].kind == "tool" && m.items[i].toolID == t.ID && m.items[i].status == "running" {
				text := m.items[i].text
				if text == "" {
					text = styleToolRun.Render(t.Name) + "\n"
				}
				text += t.Output + "\n"
				m.items[i].text = text
				break
			}
		}

	case agent.EventStatus:
		m.status = ev.Text

	case agent.EventError:
		m.items = append(m.items, logItem{kind: "error", text: ev.Text})

	case agent.EventApproval:
		m.approval = ev.Approval
		m.status = "awaiting approval…"

	case agent.EventPlan:
		if ev.Plan != nil {
			m.items = append(m.items, logItem{kind: "system", text: "── plan proposed ──"})
			m.items = append(m.items, logItem{kind: "assistant", text: ev.Plan.Plan})
			// Reuse the approval modal: y = approve & execute, n = reject.
			m.approval = &agent.ApprovalRequest{
				ID:      ev.Plan.ID,
				Tool:    "Plan",
				Command: ev.Plan.Plan,
				Reason:  "Approve this plan and start executing?",
			}
			m.status = "awaiting plan approval…"
		}

	case agent.EventPlanModeChanged:
		m.planMode = ev.PlanMode
		if ev.PlanMode {
			m.pushStatus("plan mode on")
		}

	case agent.EventTurnDone:
		m.busy = false
		m.streaming = false
		m.status = ""
		// Bell on a long turn's completion so an attention-switched user
		// notices the agent finished (Claude Code's terminal bell).
		if time.Since(m.turnStarted) > 15*time.Second {
			fmt.Fprint(os.Stdout, "\a")
		}

	case agent.EventModeChanged:
		m.mode = ev.Mode
		// "plan" permission mode also flips plan mode in the agent.
		m.planMode = ev.Mode == permissions.ModePlan

	case agent.EventHistoryCleared:
		m.items = m.items[:0]
		m.streaming = false

	case agent.EventCompacted:
		m.items = append(m.items, logItem{kind: "status", text: "context compacted"})

	case agent.EventUsage:
		if ev.Usage != nil {
			m.usage = *ev.Usage
		}

	case agent.EventHistoryChanged:
		// History was edited server-side; keep the local log in sync by
		// clearing it — the conversation is reloaded on the next turn.
		m.items = m.items[:0]
		m.pushStatus(ev.Text)

	case agent.EventSandboxChanged:
		m.pushStatus("sandbox mode updated")
	}

	m.render()
	m.viewport.GotoBottom()
	return m, m.waitEvent()
}

// handleKey routes key presses based on the current state.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Approval modal captures all keys.
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}
	// The picker captures digits/enter/esc while active.
	if m.picker != nil {
		return m.handlePickerKey(msg)
	}

	// Slash-command autocomplete popup: up/down navigate, tab accepts the
	// highlighted command, enter accepts when the input is still a prefix
	// (otherwise it submits the completed command).
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
		case "tab":
			return m.acceptCmdSuggestion(), nil
		case "enter":
			if m.textarea.Value() != "/"+m.cmdSug[m.cmdSugIdx] {
				return m.acceptCmdSuggestion(), nil
			}
			return m.submit()
		case "esc":
			m.quitArmed = false
			m.closeCmdSuggest()
			m.textarea.SetValue("")
			return m, nil
		}
	}

	switch msg.String() {
	case "ctrl+c":
		if m.busy {
			m.quitArmed = false
			m.pushStatus("interrupting agent…")
			m.ctrl <- agent.Control{Type: agent.ControlInterrupt}
			return m, nil
		}
		// Two-stage exit (Claude Code): the first press asks for
		// confirmation, the second actually quits.
		if m.quitArmed {
			m.quit = true
			return m, tea.Quit
		}
		m.quitArmed = true
		m.pushStatus("press ctrl+c again to quit")
		return m, nil

	case "pgup":
		m.quitArmed = false
		m.viewport.HalfPageUp()
		return m, nil
	case "pgdown":
		m.quitArmed = false
		m.viewport.HalfPageDown()
		return m, nil
	case "home":
		m.quitArmed = false
		m.viewport.GotoTop()
		return m, nil
	case "end":
		m.quitArmed = false
		m.viewport.GotoBottom()
		return m, nil

	case "ctrl+p":
		return m.historyPrev(), nil
	case "ctrl+n":
		return m.historyNext(), nil

	case "enter":
		return m.submit()

	case "esc":
		m.quitArmed = false
		m.closeCmdSuggest()
		m.textarea.SetValue("")
		return m, nil
	}

	// Normal typing.
	m.quitArmed = false
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refreshCmdSuggest()
	return m, cmd
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
	matches := make([]string, 0, 4)
	names := append(append([]string{}, commandNames...), m.customCommandNames()...)
	sort.Strings(names)
	for _, name := range names {
		if strings.HasPrefix(name, prefix) && (len(matches) == 0 || matches[len(matches)-1] != name) {
			matches = append(matches, name)
		}
	}
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
	m.closeCmdSuggest()
	return m
}

func (m *Model) closeCmdSuggest() {
	m.cmdSug = nil
	m.cmdSugIdx = 0
	m.syncViewportHeight()
}

// handleApprovalKey resolves an approval modal.
func (m *Model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.approval == nil {
		return m, nil
	}
	var approve, remember bool
	handled := true
	switch msg.String() {
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
		if m.approval.Tool == "Plan" {
			// Plan approval answers via ControlPlanResp (no session remember).
			m.ctrl <- agent.Control{
				Type:        agent.ControlPlanResp,
				PlanID:      m.approval.ID,
				PlanApprove: approve,
			}
			m.approval = nil
			m.status = ""
			return m, nil
		}
		m.ctrl <- agent.Control{
			Type:       agent.ControlApproval,
			ApprovalID: m.approval.ID,
			Approve:    approve,
			Remember:   remember,
		}
		if remember {
			verb := "allowed"
			if !approve {
				verb = "denied"
			}
			m.pushStatus("remembered: " + verb + " this operation for the session")
		}
		m.approval = nil
		m.status = ""
	}
	return m, nil
}

// handlePickerKey resolves the interactive list picker.
func (m *Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picker == nil {
		return m, nil
	}
	switch msg.String() {
	case "enter":
		n := 0
		for _, r := range m.picker.buf {
			n = n*10 + int(r-'0')
		}
		if n < 1 || n > len(m.picker.lines) {
			m.pushStatus("picker: enter a number between 1 and " + strconv.Itoa(len(m.picker.lines)))
			m.picker.buf = ""
			return m, nil
		}
		p := m.picker
		m.picker = nil
		p.onPick(n - 1)
	case "esc", "ctrl+c":
		m.pushStatus("picker cancelled")
		m.picker = nil
	default:
		if msg.Type == tea.KeyRunes {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, msg.String())
			if digits != "" && len(m.picker.buf) < 3 {
				m.picker.buf += digits
				m.pushStatus("pick 1–" + strconv.Itoa(len(m.picker.lines)) + " (enter confirms, esc cancels): " + m.picker.buf)
			}
		}
	}
	return m, nil
}

// startPicker activates a numbered-list picker over lines.
func (m *Model) startPicker(title string, lines []string, onPick func(int)) {
	m.picker = &pickerState{title: title, lines: lines, onPick: onPick}
	m.pushStatus(title + " (enter confirms, esc cancels)")
}

// submit handles the Enter key in the input area.
func (m *Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.textarea.Value())
	if text == "" {
		return m, nil
	}
	m.textarea.Reset()
	m.closeCmdSuggest()

	if strings.HasPrefix(text, "/") {
		return m.runCommand(text), nil
	}

	m.history = append(m.history, text)
	m.historyIdx = len(m.history)
	m.ctrl <- agent.Control{Type: agent.ControlUserMessage, Text: text}
	return m, nil
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
	m.textarea.SetValue(m.history[m.historyIdx])
	m.textarea.CursorEnd()
	m.refreshCmdSuggest()
	return m
}

func (m *Model) historyNext() tea.Model {
	if len(m.history) == 0 {
		return m
	}
	if m.historyIdx >= len(m.history)-1 {
		m.historyIdx = len(m.history)
		m.textarea.SetValue("")
		m.refreshCmdSuggest()
		return m
	}
	m.historyIdx++
	m.textarea.SetValue(m.history[m.historyIdx])
	m.textarea.CursorEnd()
	m.refreshCmdSuggest()
	return m
}

func (m *Model) pushStatus(s string) {
	m.status = s
}

func (m *Model) setStatus(format string, args ...any) {
	m.status = fmt.Sprintf(format, args...)
}
