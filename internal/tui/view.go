package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

// View renders the whole screen.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "ccdp is starting…"
	}
	if m.reader != nil {
		return m.renderReaderView()
	}

	header := m.headerPresentation()
	body := ""
	if m.inlineMode && m.followOutput {
		// Ordinary-screen mode prints completed transcript units through
		// tea.Println; only the bounded active tail is kept in the managed
		// frame. Once the user scrolls up, the full local viewport remains
		// available until they return to the bottom.
		body = (&m).inlineBody()
	} else {
		body = m.viewport.View()
	}
	status := m.renderStatus()
	footer := m.renderFooter()
	if !m.hasSnapshot {
		// A provider may deliver the first snapshot after the initial window
		// frame. Keep the required state triplet visible during that handoff as
		// well; renderFooter keeps its legacy one-row helper behavior for callers
		// that inspect the chrome before a session is attached.
		if state := m.renderPersistentStatus(); state != "" {
			footer = state + "\n" + footer
		}
	}

	decision := m.renderInlineDecision()
	picker := m.renderInlineSurface()
	suggestions := m.renderCmdSuggest()
	input := m.renderInput()

	// The ordinary screen keeps recent conversation visible while a decision or
	// selector is open. The old implementation replaced the entire frame with
	// a centered modal, which hid the model/context state and made a short
	// terminal impossible to reason about. Only the transcript is elastic;
	// controls and the persistent state line are retained by fitPresentation.
	segments := []presentationSegment{
		{text: header, mandatory: true},
		{text: body, mandatory: false},
		{text: status, mandatory: true},
		{text: decision, mandatory: true},
		{text: picker, mandatory: true},
		{text: suggestions, mandatory: true},
		{text: input, mandatory: true},
		{text: footer, mandatory: true},
	}
	return fitPresentation(segments, m.width, m.height)
}

func (m *Model) renderReaderView() string {
	if m == nil || m.reader == nil {
		return ""
	}
	width := max(1, m.width-2)
	state := m.renderPersistentStatus()
	readerTitle := styleModalTitle.Render(truncateDisplay(sanitizeANSI(m.readerHeader()), width))
	bodyHeight := max(1, m.height-presentationHeight(m.renderHeader(), width)-presentationHeight(readerTitle, width)-presentationHeight(m.readerStatus(), width)-presentationHeight(m.readerHint(), width)-presentationHeight(state, width)-1)
	body := m.readerBody(width, bodyHeight)
	segments := []presentationSegment{
		{text: m.renderHeader(), mandatory: true},
		{text: readerTitle, mandatory: true},
		{text: body, mandatory: false},
		{text: m.readerStatus(), mandatory: true},
		{text: styleHints.Render(m.readerHint()), mandatory: true},
		{text: state, mandatory: true},
	}
	return fitPresentation(segments, m.width, m.height)
}

// renderInlineDecision is presentation-only. Existing key handling and
// protocol submission continue to use the modal state, while the most recent
// transcript and the persistent footer stay on screen.
func (m *Model) renderInlineDecision() string {
	if m == nil {
		return ""
	}
	if m.question != nil {
		return m.renderQuestionInline()
	}
	if m.approval != nil {
		return m.renderApprovalInline()
	}
	return ""
}

func (m *Model) renderInlineSurface() string {
	if m == nil || m.picker == nil {
		return ""
	}
	if m.picker.action.Kind == selectorEffort {
		return m.renderEffortSelector()
	}
	// Agent navigation uses the same inline surface as model/mode selectors;
	// input focus is still routed by FocusRouter.
	start, end := m.pickerWindow()
	width := max(1, m.width-2)
	rows := make([]string, 0, end-start+2)
	rows = append(rows, styleModalTitle.Render(truncateDisplay(sanitizeANSI(m.picker.title), width)))
	for i := start; i < end; i++ {
		prefix, optionStyle := "  ", styleCmdSug
		if i == m.picker.index {
			prefix, optionStyle = "❯ ", styleCmdSugSel
		}
		label, description, pending, disabled := m.pickerOptionText(i)
		currentMark := "  "
		if m.pickerOptionCurrent(i) {
			currentMark = "✓ "
		}
		if pending {
			currentMark = "… "
		}
		if disabled {
			currentMark = "× "
			optionStyle = styleStatus
		}
		line := fmt.Sprintf("%s%s%d  %s", prefix, currentMark, i+1, label)
		if description != "" {
			line += " — " + description
		}
		if i == m.picker.index && description != "" {
			// The focused option owns a small detail expansion. This keeps the
			// selected description readable on a narrow terminal instead of
			// silently replacing it with an ellipsis; other rows stay compact.
			for _, optionLine := range wrapDisplay(sanitizeANSI(line), width) {
				rows = append(rows, optionStyle.Render(optionLine))
			}
		} else {
			rows = append(rows, optionStyle.Render(truncateDisplay(sanitizeANSI(line), width)))
		}
	}
	position := fmt.Sprintf("%d/%d", m.picker.index+1, len(m.picker.lines))
	for _, hint := range selectorHints(width, position) {
		rows = append(rows, styleHints.Render(hint))
	}
	return strings.Join(rows, "\n")
}

func (m *Model) pickerOptionText(index int) (label, description string, pending, disabled bool) {
	if m == nil || m.picker == nil || index < 0 || index >= len(m.picker.lines) {
		return "", "", false, false
	}
	if index < len(m.picker.options) {
		option := m.picker.options[index]
		label, description = sanitizeANSI(option.Label), sanitizeANSI(option.Description)
		pending, disabled = option.Pending, option.Disabled
	} else {
		label = sanitizeANSI(m.picker.lines[index])
	}
	if description == "" {
		const separatorText = " — "
		if separator := strings.Index(label, separatorText); separator >= 0 {
			description = strings.TrimSpace(label[separator+len(separatorText):])
			label = strings.TrimSpace(label[:separator])
		}
	}
	if !pending {
		pending = m.pendingPickerOption(index)
	}
	return label, description, pending, disabled
}

// pendingPickerOption projects the runtime's admitted-but-not-yet-applied
// settings into the selector. Option.Pending remains useful for callers that
// construct a selector without a session snapshot, while live selectors must
// derive this mark from the authoritative PendingSettings object.
func (m *Model) pendingPickerOption(index int) bool {
	if m == nil || !m.hasSnapshot || m.snapshot.Pending == nil || m.picker == nil ||
		index < 0 || index >= len(m.picker.options) {
		return false
	}
	optionID := m.picker.options[index].ID
	pending := m.snapshot.Pending
	switch m.picker.action.Kind {
	case selectorModel:
		return pending.Model != nil && pending.Model.Model != "" && pending.Model.Model == optionID
	case selectorMode:
		return pending.Permission != nil && pending.Permission.Mode != "" && pending.Permission.Mode == optionID
	case selectorSandbox:
		return pending.Sandbox != nil && pending.Sandbox.Mode != "" && pending.Sandbox.Mode == optionID
	case selectorPlan:
		return pending.ExecutionMode != nil && string(*pending.ExecutionMode) == optionID
	default:
		return false
	}
}

func selectorHints(width int, position string) []string {
	if width < 34 {
		return []string{"↑↓ move · enter select", "esc cancel · " + position}
	}
	return wrapDisplay("↑↓ move · enter select · esc cancel · "+position, width)
}

func (m *Model) pickerOptionCurrent(index int) bool {
	if m == nil || m.picker == nil || index < 0 || index >= len(m.picker.options) {
		return false
	}
	option := m.picker.options[index]
	if option.Current {
		return true
	}
	current := ""
	switch m.picker.action.Kind {
	case selectorModel:
		current = m.modelName
		if m.hasSnapshot {
			current = m.snapshot.Settings.Model.Model
		}
	case selectorMode:
		current = string(m.mode)
		if m.hasSnapshot && m.snapshot.Settings.Permission.Mode != "" {
			current = m.snapshot.Settings.Permission.Mode
		}
	case selectorAgent:
		current = m.sessionID
	case selectorPlan:
		if m.planMode {
			current = option.ID
		}
	}
	return current != "" && option.ID == current
}

func (m *Model) renderApprovalInline() string {
	if m == nil || m.approval == nil {
		return ""
	}
	width := max(1, m.width-2)
	tool := sanitizeANSI(m.approval.Tool)
	if tool == "" {
		tool = "action"
	}
	labelText := "◇ " + tool + " needs your decision"
	if width < 34 {
		// A narrow decision keeps its identity on one row. The full command and
		// reason remain scrollable below it, while the action rows retain both
		// one-shot and session-scoped choices.
		labelText = "◇ " + tool + " approval"
	}
	label := styleRunning.Render(labelText)
	// Keep a bounded review window in the inline decision area. The source is
	// still held by approval and the existing ↑↓ handlers update
	// approvalScroll, so a long command/plan remains inspectable without
	// replacing the recent transcript. Short terminals intentionally show the
	// first two lines and an explicit continuation hint.
	details := append([]string{}, wrapDisplay(sanitizeANSI(m.approval.Command), width)...)
	if reason := strings.TrimSpace(sanitizeANSI(m.approval.Reason)); reason != "" {
		if len(details) > 0 {
			details = append(details, "")
		}
		details = append(details, wrapDisplay(reason, width)...)
	}
	if len(details) == 0 {
		details = []string{"(no additional details)"}
	}
	budget := 2
	if width < 34 {
		budget = 1
	}
	if m.height >= 24 {
		budget = min(6, max(2, m.height/4))
	}
	maxOffset := max(0, len(details)-budget)
	offset := min(max(0, m.approvalScroll), maxOffset)
	end := min(len(details), offset+budget)
	rows := []string{label}
	if m.approvalPending {
		rows = append(rows, styleHints.Render("Submitting decision…"))
	}
	if m.modalErr != "" {
		rows = append(rows, styleToolErr.Render(truncateDisplay(sanitizeANSI(m.modalErr), width)))
	}
	rows = append(rows, details[offset:end]...)
	if maxOffset > 0 {
		rows = append(rows, styleHints.Render(fmt.Sprintf("↑↓ review · %d–%d/%d", offset+1, end, len(details))))
	}
	if tool == "Plan" {
		rows = append(rows, styleHints.Render(truncateDisplay("enter approve · esc decline", width)))
	} else {
		for _, hint := range approvalHints(width) {
			rows = append(rows, styleHints.Render(hint))
		}
	}
	return strings.Join(rows, "\n")
}

func approvalHints(width int) []string {
	if width < 34 {
		return []string{"y allow · n deny · esc cancel", "a allow session · x deny session"}
	}
	return wrapDisplay("y allow once · a allow for session · n deny · x deny for session · esc cancel", width)
}

func (m *Model) renderQuestionInline() string {
	if m == nil || m.question == nil || m.question.request == nil || len(m.question.request.Questions) == 0 {
		return ""
	}
	s := m.question
	index := min(max(0, s.index), len(s.request.Questions)-1)
	q := s.request.Questions[index]
	width := max(1, m.width-2)
	rows := []string{styleRunning.Render(truncateDisplay(fmt.Sprintf("◇ %s · %d/%d", q.Header, index+1, len(s.request.Questions)), width))}
	body := wrapDisplay(sanitizeANSI(q.Question), width)
	focusRow := len(body)
	for i, option := range q.Options {
		mark := "○"
		if q.MultiSelect {
			mark = "□"
		}
		if index < len(s.answers) {
			for _, selected := range s.answers[index].Selected {
				if selected == option.Label {
					if q.MultiSelect {
						mark = "▣"
					} else {
						mark = "●"
					}
				}
			}
		}
		cursor := "  "
		if i == s.cursor && !s.editing {
			cursor = "❯ "
		}
		optionRows := wrapDisplay(cursor+mark+" "+sanitizeANSI(option.Label)+" — "+sanitizeANSI(option.Description), width)
		if i == s.cursor && !s.editing {
			focusRow = len(body)
		}
		body = append(body, optionRows...)
	}
	if len(body) == 0 {
		body = []string{"(no options)"}
	}
	// The question body is independently scrollable. Keep the header and
	// controls anchored while long descriptions move underneath the cursor;
	// this makes PgUp/PgDn and the existing state.scroll field meaningful in
	// the inline surface rather than relying on the outer frame to clip them.
	bodyRows := max(2, m.height-8)
	if m.height <= 0 {
		bodyRows = 6
	}
	bodyRows = min(bodyRows, len(body))
	offset := min(max(0, s.scroll), max(0, len(body)-bodyRows))
	if s.scroll < 0 {
		offset = min(max(0, focusRow-bodyRows+1), max(0, len(body)-bodyRows))
	}
	if focusRow < offset {
		offset = focusRow
	} else if focusRow >= offset+bodyRows {
		offset = focusRow - bodyRows + 1
	}
	offset = min(max(0, offset), max(0, len(body)-bodyRows))
	if offset > 0 {
		rows = append(rows, styleHints.Render(fmt.Sprintf("↑ more · %d–%d/%d", offset+1, min(len(body), offset+bodyRows), len(body))))
	}
	rows = append(rows, body[offset:min(len(body), offset+bodyRows)]...)
	if len(body)-offset > bodyRows {
		rows = append(rows, styleHints.Render(fmt.Sprintf("↓ more · %d–%d/%d", offset+1, min(len(body), offset+bodyRows), len(body))))
	}
	if s.editing {
		rows = append(rows, truncateDisplay(s.input.View(), width))
	}
	if s.pending != "" {
		rows = append(rows, styleHints.Render("Submitting answer…"))
	} else {
		for _, hint := range questionHints(width) {
			rows = append(rows, styleHints.Render(hint))
		}
	}
	if s.errorText != "" {
		for _, line := range wrapDisplay(sanitizeANSI(s.errorText), width) {
			rows = append(rows, styleToolErr.Render(line))
		}
	}
	return strings.Join(rows, "\n")
}

func questionHints(width int) []string {
	if width < 34 {
		return []string{"↑↓ choose · space select", "tab text · enter · esc"}
	}
	return wrapDisplay("↑↓ choose · Space select · Tab text · Enter next · Esc cancel", width)
}

func (m *Model) pickerVisibleRows() int {
	if m.picker == nil {
		return 0
	}
	rows := min(maxPickerRows, len(m.picker.lines))
	if m.height > 0 {
		rows = min(rows, max(1, m.height-9))
	}
	return rows
}

func (m *Model) pickerWindow() (int, int) {
	visibleRows := m.pickerVisibleRows()
	if visibleRows <= 0 || len(m.picker.lines) == 0 {
		return 0, 0
	}
	m.picker.selector.SetSize(visibleRows)
	start := m.picker.selector.Scroll.Offset
	end := min(len(m.picker.lines), start+visibleRows)
	if end-start < visibleRows {
		start = max(0, end-visibleRows)
	}
	return start, end
}

func (m *Model) renderInlinePicker() string {
	if m.picker == nil || !m.picker.inline || len(m.picker.lines) == 0 {
		return ""
	}
	start, end := m.pickerWindow()
	width := m.width - 4
	if width <= 0 {
		width = 80
	}
	labelWidth := max(1, width-2)
	var body strings.Builder
	body.WriteString(styleModalTitle.Render(sanitizeANSI(m.picker.title)))
	body.WriteString("\n")
	for i := start; i < end; i++ {
		prefix := "  "
		lineStyle := styleCmdSug
		if i == m.picker.index {
			prefix = "❯ "
			lineStyle = styleCmdSugSel
		}
		body.WriteString(prefix + lineStyle.Render(truncateDisplay(sanitizeANSI(m.picker.lines[i]), labelWidth)) + "\n")
	}
	position := fmt.Sprintf("%d/%d", m.picker.index+1, len(m.picker.lines))
	// Keep the inline surface useful on a narrow terminal.  The old hint was
	// wider than the terminal and was wrapped by the root view, stealing rows
	// from the composer.  All controls remain represented by this compact line.
	hint := truncateDisplay("↑↓ select · enter confirm · esc cancel · "+position, width)
	body.WriteString(styleStatus.Render(hint))
	body.WriteString("\n")
	return body.String()
}

func (m *Model) renderHeader() string {
	modelName := m.modelName
	mode := m.mode
	planMode := m.planMode
	sessionID := m.sessionID
	usage := m.usage
	contextWindow := 0
	contextUsed := usage.InputTokens + usage.OutputTokens
	if m.hasSnapshot {
		modelName = m.snapshot.Settings.Model.Model
		if m.snapshot.Settings.Permission.Mode != "" {
			mode = permissions.Mode(m.snapshot.Settings.Permission.Mode)
		}
		planMode = m.snapshot.Settings.ExecutionMode == protocol.ExecutionModePlan
		sessionID = m.snapshot.SessionID.String()
		usage = agentUsageFromSnapshot(m.snapshot.Usage)
		contextWindow = m.snapshot.Settings.ContextWindow
		contextUsed = m.snapshot.ContextUsedTokens
	}
	var modeColor lipgloss.TerminalColor = colorMuted
	switch mode {
	case "acceptEdits":
		modeColor = colorGo
	case "bypassPermissions":
		modeColor = colorError
	}
	modeBadge := lipgloss.NewStyle().Foreground(modeColor).Bold(true).Render(string(mode))

	// Plan mode gets its own amber badge.
	if planMode {
		planBadge := styleRunning.Render("plan")
		modeBadge = modeBadge + " " + planBadge
	}

	ws := truncateDisplay(sanitizeANSI(m.workspace), 24)

	hasCost := usage.TurnCount > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0
	parts := make([]string, 0, len(m.statusItems))
	items := m.statusItems
	if sameStatusItems(items, defaultStatusItems) {
		// The persistent footer owns model/effort/context/cost. Keep the header
		// empty after the welcome item, so a normal conversation does not repeat
		// launch identity or settings above and below the transcript.
		items = nil
	}
	for _, item := range items {
		switch item {
		case "version":
			parts = append(parts, styleBrand.Render("ccdp")+" v"+appVersion)
		case "model":
			parts = append(parts, styleAssistant.Render(truncateDisplay(sanitizeANSI(modelName), max(1, m.width))))
		case "mode":
			parts = append(parts, "mode:"+modeBadge)
		case "session":
			parts = append(parts, "session:"+shortID(sanitizeANSI(sessionID)))
		case "workspace":
			parts = append(parts, ws)
		case "cost":
			if hasCost {
				parts = append(parts, fmt.Sprintf("%.1fk→%.1fk · $%.3f",
					float64(usage.InputTokens)/1000,
					float64(usage.OutputTokens)/1000,
					usage.Cost))
			}
		case "context":
			// Keep the runtime's authoritative context estimate visible. The
			// usage counters are request accounting, not the live context
			// projection, so a snapshot always wins above.
			window := contextWindow
			if window <= 0 {
				break
			}
			used := contextUsed
			if used < 0 {
				used = 0
			}
			style := styleContextOK
			if float64(used) >= float64(window)*0.8 {
				style = styleContextWarn
			}
			parts = append(parts, style.Render(formatContextTokens(used)+"/"+formatContextTokens(window)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fitLines(styleHeader.Render(strings.Join(parts, "  ·  ")), m.width)
}

func sameStatusItems(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// formatContextTokens keeps the context indicator compact without hiding
// zero, sub-percent, or over-capacity estimates. It is intentionally only a
// kilo suffix: the full token count remains legible for ordinary model
// windows and no precision is lost below 1000 tokens.
func formatContextTokens(tokens int) string {
	if tokens < 0 {
		tokens = 0
	}
	if tokens < 1000 {
		return strconv.Itoa(tokens)
	}
	value := strconv.FormatFloat(float64(tokens)/1000, 'f', 1, 64)
	value = strings.TrimSuffix(value, ".0")
	return value + "k"
}

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
	status := truncateDisplay(sanitizeANSI(m.status), width)
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
				status = "正在处理"
			}
		}
		elapsed := ""
		if !m.turnStarted.IsZero() {
			elapsed = " · " + formatElapsed(time.Since(m.turnStarted))
		}
		spinner := m.spinner.View()
		if !m.activity.Active() && m.activity.Phase == ActivityIdle {
			// No phase event means no evidence for an animated thought state.
			spinner = "·"
		}
		return " " + fitLines(spinner+" "+styleRunning.Render(status)+styleHints.Render(elapsed), width)
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
		// Completion is represented by the final tool/assistant rows and the
		// persistent footer. A permanent “All set” row only pushes useful
		// transcript and input content down on short terminals.
		return ""
	}
	return ""
}

func formatElapsed(elapsed time.Duration) string {
	seconds := max(0, int(elapsed.Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}

func (m *Model) renderInput() string {
	// A full-width, quiet rule keeps the input easy to find as output streams.
	border := colorGo
	if m.busy {
		border = colorBorder
	}
	composer := m.textarea.View()
	if m.pasteFold != nil && !m.pasteFold.Expanded {
		// The full literal remains in textarea.Value for submit/copy, but a
		// collapsed paste should not occupy five ordinary composer rows. Ctrl+E
		// switches back to the editable textarea without touching that source.
		composer = styleStatus.Render(truncateDisplay("  "+m.pasteFold.summary(), max(1, m.width-4)))
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true, false, false, false).
		BorderForeground(border).
		Padding(0, 1).
		MaxWidth(m.width).
		Render(m.imageInputSummary() + composer)
}

// renderCmdSuggest draws the slash-command autocomplete popup above the input
// while the user is typing a "/" command. The highlighted entry carries a ❯.
func (m *Model) renderCmdSuggest() string {
	if len(m.cmdSug) == 0 {
		return ""
	}
	start, end := m.cmdSuggestionWindow()
	visible := m.cmdSug[start:end]
	available := 72
	if m.width > 0 {
		available = max(4, m.width-6)
	}
	maxName := 0
	for _, name := range visible {
		if l := lipgloss.Width("/" + name); l > maxName {
			maxName = l
		}
	}
	maxName = min(maxName, max(1, available-4))
	var sb strings.Builder
	for i, name := range visible {
		absolute := start + i
		label := truncateDisplay("/"+name, maxName)
		label += strings.Repeat(" ", max(0, maxName-lipgloss.Width(label)))
		description := ""
		if entry, ok := commandCatalog.Lookup(name); ok {
			description = entry.Help
		} else if custom := m.findCustomCommand(name); custom != nil {
			description = "custom command · " + custom.source + " scope"
		}
		remaining := max(1, available-2-maxName-2)
		if description != "" {
			description = truncateDisplay(description, remaining)
		}
		line := label
		if description != "" {
			line += "  " + styleStatus.Render(description)
		}
		if absolute == m.cmdSugIdx {
			line = "❯ " + styleCmdSugSel.Render(line)
		} else {
			line = "  " + styleCmdSug.Render(line)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if len(m.cmdSug) > len(visible) {
		sb.WriteString("  " + styleCmdSug.Render(fmt.Sprintf("↑/↓ %d–%d of %d  ·  fn+↑/↓ jump",
			start+1, end, len(m.cmdSug))))
		sb.WriteString("\n")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Render(strings.TrimRight(sb.String(), "\n")) + "\n"
}

func (m *Model) cmdSuggestionWindow() (start, end int) {
	pageSize := m.cmdSuggestionPageSize()
	end = min(len(m.cmdSug), pageSize)
	if m.cmdSugIdx >= end {
		end = min(len(m.cmdSug), m.cmdSugIdx+1)
		start = end - pageSize
	}
	return max(0, start), end
}

func (m *Model) renderFooter() string {
	if m.question != nil || m.approval != nil || m.picker != nil {
		// Modal-owned keys are intercepted before the composer. Repeating
		// “enter send” underneath an inline decision would describe an action
		// that cannot happen; the decision/selector surface already renders its
		// focused key hints next to the content.
		return m.renderPersistentStatus()
	}
	hints := "enter send · ⌥enter newline · ⌃V paste · / commands"
	if m.busy {
		hints = "⌃C interrupt · enter send follow-up · / commands"
	} else if m.quitArmed {
		hints = "Press ⌃C again to exit · any other key to continue"
	} else if len(m.cmdSug) > 0 {
		hints = "↑↓ choose · tab complete · enter run · esc close"
	}
	if m.width < 52 {
		hints = "enter send · /help"
		if m.busy {
			hints = "⌃C interrupt · enter send"
		} else if m.quitArmed {
			hints = "⌃C again to exit"
		} else if len(m.cmdSug) > 0 {
			hints = "↑↓ choose · tab · esc"
		}
	}
	hintLine := " " + styleHints.Render(truncateDisplay(hints, max(1, m.width-2)))
	state := m.renderPersistentStatus()
	if state == "" || !m.hasSnapshot {
		return hintLine
	}
	return state + "\n" + hintLine
}

// renderPersistentStatus is the compact, authoritative session footer. It is
// deliberately independent of /statusline: users must be able to see the
// active model, confirmed reasoning effort, and context estimate while a
// selector, approval, question, or reader is open. Pending settings never
// replace the confirmed snapshot here.
func (m *Model) renderPersistentStatus() string {
	if m == nil {
		return ""
	}
	modelName := m.modelName
	effort := ""
	used := 0
	window := 0
	if m.hasSnapshot {
		modelName = m.snapshot.Settings.Model.Model
		used = m.snapshot.ContextUsedTokens
		window = m.snapshot.Settings.ContextWindow
		effort = strings.TrimSpace(m.snapshot.Settings.ReasoningEffort)
	}
	if modelName == "" {
		modelName = "model"
	}
	if effort == "" {
		effort = "default"
	}
	if used < 0 {
		used = 0
	}
	ratio := "—/—"
	if m.hasSnapshot {
		ratio = formatContextTokens(used) + "/—"
	}
	if window > 0 {
		ratio = formatContextTokens(used) + "/" + formatContextTokens(window)
	}
	mode := string(m.mode)
	if mode == "" {
		mode = "default"
	}
	if m.snapshot.Settings.Permission.Mode != "" {
		mode = string(m.snapshot.Settings.Permission.Mode)
	}
	if m.planMode {
		mode += " · plan"
	}
	modelName = compactModelName(sanitizeANSI(modelName), max(1, m.width))
	// The default permission mode adds no information in the compact footer;
	// omitting it is what lets the required model/effort/context triplet stay on
	// one row at ordinary narrow widths. Non-default permissions and plan mode
	// remain explicit because silently dropping them could change the decision a
	// user is about to make.
	parts := make([]string, 0, 4)
	if mode != string(permissions.ModeDefault) {
		parts = append(parts, mode)
	}
	parts = append(parts, modelName, effort)
	ratioStyle := styleContextOK
	if window > 0 && float64(used) >= float64(window)*0.8 {
		ratioStyle = styleContextWarn
	}
	parts = append(parts, ratioStyle.Render(ratio))
	line := styleStatus.Render(strings.Join(parts[:len(parts)-1], " · ")) + " · " + parts[len(parts)-1]
	return wrapPersistentStatus(line, max(1, m.width-2))
}

func compactModelName(name string, width int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "model"
	}
	// Provider prefixes are useful in details but consume the exact cells that
	// make the footer readable on a narrow terminal. Keep the final component
	// when a slash-qualified name cannot fit as a whole.
	if lipgloss.Width(name) > width/2 && strings.Contains(name, "/") {
		parts := strings.Split(name, "/")
		name = parts[len(parts)-1]
	}
	return truncateDisplay(name, max(1, width/2))
}

func (m *Model) renderApproval() string {
	req := m.approval
	if req == nil {
		return ""
	}
	titleText := m.approvalTitleText()
	width := m.approvalModalWidth()
	innerWidth := m.approvalContentWidth()
	// A title is a label, not the review payload. Keep it to one bounded row on
	// a narrow terminal so the decision controls remain visible; the complete
	// tool/plan text is still available in the scrollable details below.
	titleLines := []string{truncateDisplay(titleText, innerWidth)}
	details := m.approvalDetailLines()
	visibleRows := m.approvalVisibleRows()
	m.approvalState.Offset = m.approvalScroll
	m.approvalState.Set(visibleRows, len(details))
	m.approvalScroll = m.approvalState.Offset
	maxOffset := m.approvalState.MaxOffset()
	end := min(len(details), m.approvalState.Offset+visibleRows)

	var body strings.Builder
	for _, line := range titleLines {
		body.WriteString(styleModalTitle.Render(line))
		body.WriteString("\n")
	}
	if innerWidth >= 20 {
		body.WriteString("\n")
	}
	for _, line := range details[m.approvalState.Offset:end] {
		body.WriteString(line + "\n")
	}
	if maxOffset > 0 {
		hint := fmt.Sprintf("↑↓ review · %d–%d of %d", m.approvalState.Offset+1, end, len(details))
		body.WriteString(styleStatus.Render(truncateDisplay(hint, innerWidth)))
		body.WriteString("\n")
	}
	if innerWidth >= 20 {
		body.WriteString("\n")
	}
	decisionLines := approvalDecisionLines(innerWidth)
	if m.approval.Tool == "Plan" {
		decisionLines = approvalPlanDecisionLines(innerWidth)
	}
	for _, line := range decisionLines {
		body.WriteString(line)
		body.WriteString("\n")
	}
	return styleModal.Width(width).Render(strings.TrimRight(body.String(), "\n"))
}

func (m *Model) approvalModalWidth() int {
	if m.width <= 0 {
		return 56
	}
	return min(56, max(10, m.width-6))
}

func (m *Model) approvalVisibleRows() int {
	details := m.approvalDetailLines()
	if len(details) == 0 {
		return 1
	}
	title := 1
	decisions := len(approvalDecisionLines(m.approvalContentWidth()))
	if m.approval != nil && m.approval.Tool == "Plan" {
		decisions = len(approvalPlanDecisionLines(m.approvalContentWidth()))
	}
	spacers := 0
	if m.approvalContentWidth() >= 20 {
		spacers = 2
	}
	// styleModal contributes one border and one padding row on each side.
	available := m.height - 4 - title - decisions - spacers
	if m.height <= 0 {
		available = 10
	}
	if len(details) > max(1, available) {
		// The position hint occupies a row only while there is overflow.
		available--
	}
	return max(1, available)
}

func (m *Model) approvalContentWidth() int {
	// One border cell plus two padding cells on each side are outside the
	// content width of styleModal. Keeping details within this width prevents
	// Lipgloss from wrapping them a second time at render time.
	return max(1, m.approvalModalWidth()-6)
}

func (m *Model) approvalTitleText() string {
	if m.approval == nil {
		return ""
	}
	if m.approval.Tool == "Plan" {
		return "📋  Plan ready — approve?"
	}
	return fmt.Sprintf("⚠  %s requires approval", m.approval.Tool)
}

func approvalDecisionLines(width int) []string {
	if width <= 0 {
		width = 1
	}
	raw := []string{
		"[y] allow    [n] deny",
		"[a] always   [x] never",
		"[esc] deny",
	}
	var lines []string
	for _, line := range raw {
		lines = append(lines, wrapDisplay(line, width)...)
	}
	return lines
}

func approvalPlanDecisionLines(width int) []string {
	if width <= 0 {
		width = 1
	}
	var lines []string
	for _, line := range []string{"[enter] approve plan", "[esc] decline"} {
		lines = append(lines, wrapDisplay(line, width)...)
	}
	return lines
}

func (m *Model) approvalDetailLines() []string {
	if m.approval == nil {
		return nil
	}
	width := max(1, m.approvalContentWidth())
	command := wrapDisplay(sanitizeANSI(m.approval.Command), width)
	reason := wrapDisplay(sanitizeANSI(m.approval.Reason), width)
	lines := append([]string{}, command...)
	if len(command) > 0 && len(reason) > 0 {
		lines = append(lines, "")
	}
	for _, line := range reason {
		lines = append(lines, styleStatus.Render(line))
	}
	return lines
}

func (m *Model) approvalMaxOffset() int {
	return max(0, len(m.approvalDetailLines())-m.approvalVisibleRows())
}

func wrapDisplay(s string, width int) []string {
	if s == "" {
		return nil
	}
	wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(s)
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func (m *Model) renderPicker() string {
	if m.picker == nil || len(m.picker.lines) == 0 {
		return ""
	}
	// There is no room for a bordered/padded modal on a very small terminal.
	// Keep the selector usable and bounded instead of allowing lipgloss to
	// produce a surface taller than the terminal.
	if m.height > 0 && m.height <= 10 {
		width := max(1, m.width-2)
		compact := fmt.Sprintf("%s · %d/%d %s", sanitizeANSI(m.picker.title), m.picker.index+1,
			len(m.picker.lines), sanitizeANSI(m.picker.lines[m.picker.index]))
		return truncateDisplay(compact, width)
	}
	start, end := m.pickerWindow()

	modalWidth := 64
	if m.width > 0 {
		modalWidth = min(modalWidth, max(10, m.width-6))
	}
	textWidth := max(8, modalWidth-10)
	var body strings.Builder
	body.WriteString(styleModalTitle.Render(sanitizeANSI(m.picker.title)))
	body.WriteString("\n\n")
	for i := start; i < end; i++ {
		prefix := "  "
		lineStyle := styleCmdSug
		if i == m.picker.index {
			prefix = "❯ "
			lineStyle = styleCmdSugSel
		}
		line := fmt.Sprintf("%*d  %s", len(strconv.Itoa(len(m.picker.lines))), i+1,
			truncateDisplay(sanitizeANSI(m.picker.lines[i]), textWidth))
		body.WriteString(prefix + lineStyle.Render(line) + "\n")
	}
	body.WriteString("\n")
	position := fmt.Sprintf("%d/%d", m.picker.index+1, len(m.picker.lines))
	if m.picker.buf != "" {
		position += "  number: " + m.picker.buf
	}
	body.WriteString(styleStatus.Render("↑/↓ navigate · fn+↑/↓ jump · enter select · esc cancel · " + position))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorGo).
		Padding(1, 2).
		Width(modalWidth).
		Render(strings.TrimRight(body.String(), "\n"))
}

func truncateDisplay(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	var out strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(s)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := lipgloss.Width(cluster)
		if used+clusterWidth+1 > width {
			break
		}
		out.WriteString(cluster)
		used += clusterWidth
	}
	return out.String() + "…"
}

// render draws the conversation log into the viewport. Item text is
// sanitized once per item (cached in the item) so escape sequences embedded
// in tool output or model text can never corrupt the display, while
// lipgloss's own styling sequences are applied afterwards and survive.
func (m *Model) render() {
	var sb strings.Builder
	contentWidth := m.viewport.Width - m.viewport.Style.GetHorizontalFrameSize()
	for i := range m.items {
		if i > 0 && transcriptGap(m.items[i-1].kind, m.items[i].kind) {
			sb.WriteString("\n")
		}
		item := m.renderLogItem(&m.items[i], contentWidth)
		if contentWidth > 0 {
			// The viewport clips horizontally and has no horizontal scrollbar.
			// Wrap every transcript item so long errors, commands and model output
			// remain readable and vertically scrollable.
			item = lipgloss.NewStyle().Width(contentWidth).Render(item)
		}
		sb.WriteString(item)
		sb.WriteString("\n")
	}
	m.viewport.SetContent(sb.String())
	// Keep the shared scroll state in sync with the legacy viewport projection.
	// The selector and approval surfaces use the same bounds model directly;
	// mirroring here means a resize or a native-scroll handoff cannot retain a
	// stale transcript offset.
	m.transcriptScroll.Set(m.viewport.Height, m.viewport.TotalLineCount())
	m.transcriptScroll.Offset = min(m.viewport.YOffset, m.transcriptScroll.MaxOffset())
}

func renderItem(it *logItem) string {
	return renderItemWidth(it, 80)
}

func renderItemWidth(it *logItem, width int) string {
	// Strip terminal escape sequences embedded in the message text itself,
	// once per item version; the lipgloss styling around it is applied after
	// this point. Streaming appends grow it.text, so a stale cache is
	// detected by length and recomputed.
	// Length alone is not a content version. Snapshot reconciliation can
	// replace a message with an equal-length correction (for example OLD →
	// NEW), so compare the cached source as well before reusing the sanitized
	// projection. ANSI-bearing sources intentionally take the cheap sanitize
	// path again; correctness is more important than retaining a stale style.
	if it.sanitizedLen != len(it.text) || it.sanitized != it.text {
		it.sanitized = sanitizeANSI(it.text)
		it.sanitizedLen = len(it.text)
	}
	text := it.sanitized
	switch it.kind {
	case "welcome":
		return renderWelcome(text, width)

	case "user":
		// Match the composer and the target layout: a single colored prompt
		// marker distinguishes user input without spending a row on "You".
		text = strings.ReplaceAll(text, "\n", "\n  ")
		return styleUser.Render("❯ ") + text

	case "command":
		return styleUser.Render("❯ " + text)

	case "assistant":
		// Keep assistant paragraphs on a stable two-cell column. The source
		// remains untouched; only the display projection receives the indent.
		return indentAssistant(renderMarkdown(text, max(1, width-2)))

	case "thinking":
		return styleStatus.Italic(true).Render("∴ " + text)

	case "tool":
		return renderToolView(it, width)

	case "error":
		return styleError.Render("✗ " + text)

	case "status":
		return styleStatus.Render("· " + text)

	case "system":
		return styleSystem.Render(text)

	default:
		return text
	}
}

func indentAssistant(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// renderToolText produces the plain-text display body of a tool entry. It
// reports whether the second line is the command/path/args meta line, so
// renderItem can color those lines itself after sanitization (styles baked in
// here would be stripped by sanitizeANSI and never reach the terminal).
func renderToolText(name string, args map[string]any, status, output string) (string, bool) {
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString("\n")
	meta := false
	if cmd, ok := args["command"].(string); ok {
		sb.WriteString("$ " + cmd)
		sb.WriteString("\n")
		meta = true
	} else if p, ok := args["file_path"].(string); ok {
		sb.WriteString(p)
		sb.WriteString("\n")
		meta = true
	} else if len(args) > 0 {
		// Compact one-line summary for other argument shapes.
		summary, err := json.Marshal(args)
		if err == nil && len(summary) <= 120 {
			sb.WriteString(string(summary))
			sb.WriteString("\n")
			meta = true
		}
	}
	if output != "" {
		sb.WriteString(strings.TrimRight(output, "\n"))
	}
	return sb.String(), meta
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}
