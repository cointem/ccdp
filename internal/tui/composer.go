package tui

import (
	"ccdp/internal/commands"
	"ccdp/internal/protocol"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"sort"
	"strings"
)

const composerPlaceholder = "Ask CCDP to do anything"

// newTextarea builds the input textarea with ccdp's key bindings: Enter
// submits (handled by handleKey), Alt+Enter / Ctrl+J insert a newline. Terminals
// cannot report plain Shift+Enter, so the promise moves to these keys.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = composerPlaceholder
	ta.Prompt = "› "
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "› "
		}
		return "  "
	})
	ta.FocusedStyle.Base = lipgloss.NewStyle().Background(colorInputBackground)
	ta.FocusedStyle.Prompt = styleUser.Bold(true)
	ta.FocusedStyle.Text = styleAssistant
	ta.FocusedStyle.Placeholder = styleHints
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.Style = styleUser.Background(colorInputBackground)
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

// desiredInputHeight grows the composer for explicit newlines and wrapped
// text, while keeping an empty/single-line prompt compact.
func (m *composerState) desiredInputHeight() int {
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

// handleKey routes key presses based on the current state.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.detail != nil {
		return m.handleDetailKey(msg)
	}
	if model, cmd, handled := m.manager.Route(m, msg); handled {
		return model, cmd
	}
	// ctrl+o expands the transcript in the detail reader, matching the sibling
	// CLIs' verbose-output shortcut. The agent catalog stays on /agents and the
	// ctrl+←/→ cycling keys, so it needs no dedicated hotkey here.
	if msg.String() == "ctrl+o" {
		return m, m.openTranscriptReader()
	}
	// ctrl+r opens incremental reverse search over the input history. It is a
	// no-op when there is no history or a modal already owns the keys.
	if msg.String() == "ctrl+r" {
		if m.startHistorySearch() {
			return m, nil
		}
	}
	// ctrl+t toggles the working task checklist. The panel is hidden when the
	// session has no tasks, so the toggle is a cheap no-op until the agent has
	// produced a todo list.
	if msg.String() == "ctrl+t" {
		m.quitArmed = false
		m.tasksVisible = !m.tasksVisible
		return m, nil
	}
	// ctrl+g edits the current draft in the external $EDITOR.
	if msg.String() == "ctrl+g" {
		if cmd := m.openExternalEditor(); cmd != nil {
			m.quitArmed = false
			return m, cmd
		}
	}
	// ctrl+a opens the agent picker to switch into any subagent.
	if msg.String() == "ctrl+a" {
		m.quitArmed = false
		if m.routing != nil {
			return m, m.openAgentPicker()
		}
	}
	// ctrl+k toggles detailed context breakdown.
	if msg.String() == "ctrl+k" {
		m.quitArmed = false
		m.showContextDetail = !m.showContextDetail
		return m, nil
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
		m.refreshFileMention()
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

	// @-completion popup: up/down navigate, Tab/Enter insert the highlighted
	// path, Esc closes. Typing falls through so the query keeps filtering.
	if m.fileMention != nil {
		switch msg.String() {
		case "up":
			m.quitArmed = false
			m.moveFileMention(-1)
			return m, nil
		case "down":
			m.quitArmed = false
			m.moveFileMention(1)
			return m, nil
		case "tab":
			m.acceptFileMention()
			return m, nil
		case "enter":
			m.acceptFileMention()
			return m, nil
		case "esc":
			m.quitArmed = false
			m.closeFileMention()
			return m, nil
		}
	}

	switch msg.String() {
	case "ctrl+c":
		if m.busy {
			m.quitArmed = false
			return m, m.requestInterrupt()
		}
		// Two-stage exit (Claude Code): the first press asks for
		// confirmation, the second actually quits.
		if m.quitArmed {
			return m, m.requestQuit()
		}
		m.quitArmed = true
		m.pushStatus("press ctrl+c again to quit")
		return m, nil

	case "pgup", "ctrl+u":
		if m.inlineMode && m.followOutput {
			m.render()
		}
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
			// The ordinary-screen transcript lives in native scrollback.
			// Switching to the full viewport here changes the whole frame even
			// when there is no input history to navigate.
			if m.inlineMode {
				return m, nil
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
			if m.inlineMode {
				return m, nil
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

	case "ctrl+left":
		// Quick-switch between sibling agents in spawn order. Falls through to
		// the composer when there is no agent tree to cycle.
		if cmd := m.cycleAgent(-1); cmd != nil {
			m.quitArmed = false
			return m, cmd
		}
	case "ctrl+right":
		if cmd := m.cycleAgent(1); cmd != nil {
			m.quitArmed = false
			return m, cmd
		}

	case "shift+tab":
		// Cycle the permission mode in place, matching the sibling CLIs, without
		// opening /mode. The change applies immediately, even while a turn runs;
		// the in-flight request keeps its frozen mode and the next one sees the
		// new setting.
		if cmd := m.cyclePermissionMode(); cmd != nil {
			m.quitArmed = false
			return m, cmd
		}

	case "enter":
		return m.submit()

	case "esc":
		m.quitArmed = false
		if m.activePanel() == panelTasks {
			m.tasksVisible = false
			return m, nil
		}
		if m.showContextDetail {
			m.showContextDetail = false
			return m, nil
		}
		if m.routing != nil && m.sessionID != m.routing.rootID {
			return m, m.openAgentView(m.parentAgentID())
		}
		if m.busy {
			return m, m.requestInterrupt()
		}
		m.closeCmdSuggest()
		// Esc dismisses temporary UI while preserving the root draft and its
		// attachments. A pending clipboard request is cancelled by invalidating
		// its token, so a late host result cannot resurrect an attachment.
		m.pasteRequest = ""
		return m, nil
	}

	// Give edits the full growth budget before the textarea scrolls its cursor
	// into view. Growing only after Update leaves its viewport scrolled past
	// the first line when a one-line draft gains a newline.
	m.quitArmed = false
	m.expandPasteFoldForEdit()
	previousHeight := m.textarea.Height()
	m.textarea.SetHeight(maxInputLines)
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.textarea.SetHeight(previousHeight)
	m.refreshPasteFold()
	m.syncInputHeight()
	m.refreshCmdSuggest()
	m.refreshFileMention()
	return m, cmd
}

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

// submit handles the Enter key in the input area.
func (m *Model) submit() (tea.Model, tea.Cmd) {
	return m.submitInput(protocol.InputSteer)
}

func (m *Model) submitInput(strategy protocol.InputStrategy) (tea.Model, tea.Cmd) {
	raw := m.textarea.Value()
	if m.watchDisconnected && !strings.HasPrefix(strings.TrimSpace(raw), "/") {
		m.pushStatus("connection unavailable; draft preserved · /reconnect")
		return m, nil
	}
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
	m.closeFileMention()
	m.history = append(m.history, text)
	m.historyIdx = len(m.history)
	m.followOutput = true
	// The line above the composer is ephemeral: every new submission either
	// replaces it with fresh status or clears it before producing durable output.
	m.clearTransientNotices()

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
	cmd := protocol.NewSubmitInput(commandID, protocol.SessionID(m.sessionID), protocol.InputID(commandID), text, strategy)
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
