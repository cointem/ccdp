package tui

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
)

func (m *Model) renderInlineSurface() string {
	if m == nil || m.picker == nil || len(m.picker.Options) == 0 {
		return ""
	}
	if m.height > 0 && m.height <= 10 {
		return truncateDisplay(fmt.Sprintf("%s · %d/%d %s", sanitizeANSI(m.picker.Title),
			m.picker.Index+1, len(m.picker.Options), sanitizeANSI(m.picker.Options[m.picker.Index].Label)), max(1, m.width-2))
	}
	if m.picker.action.Kind == selectorEffort {
		return m.renderEffortSelector()
	}
	// Agent navigation uses the same inline surface as model/mode selectors;
	// input focus is still routed by PanelManager.
	start, end := m.pickerWindow()
	width := max(1, m.width-2)
	rows := make([]string, 0, end-start+2)
	rows = append(rows, styleModalTitle.Render(truncateDisplay(sanitizeANSI(m.picker.Title), width)))
	if m.height >= 22 {
		rows = append([]string{styleDivider.Render(strings.Repeat("─", width)), rows[0], ""}, rows[1:]...)
	}
	for i := start; i < end; i++ {
		prefix, optionStyle := "  ", styleCmdSug
		if i == m.picker.Index {
			prefix, optionStyle = "❯ ", styleCmdSugSel
		}
		label, description, disabled := m.pickerOptionText(i)
		currentMark := "  "
		if m.pickerOptionCurrent(i) {
			currentMark = "✓ "
		}
		if disabled {
			currentMark = "× "
			optionStyle = styleStatus
		}
		line := fmt.Sprintf("%s%s%d  %s", prefix, currentMark, i+1, label)
		if description != "" {
			line += " — " + description
		}
		if i == m.picker.Index && description != "" {
			// The focused option owns a small detail expansion. This keeps the
			// selected description readable on a narrow terminal instead of
			// silently replacing it with an ellipsis; other rows stay compact.
			optionLines := wrapDisplay(sanitizeANSI(line), width)
			for _, optionLine := range optionLines {
				rows = append(rows, optionStyle.Width(width).Render(optionLine))
			}
		} else {
			rows = append(rows, optionStyle.Width(width).Render(truncateDisplay(sanitizeANSI(line), width)))
		}
	}
	if m.height >= 22 {
		rows = append(rows, "")
	}
	if m.modalErr != "" {
		rows = append(rows, styleToolErr.Render(truncateDisplay(m.modalErr, width)))
	}
	position := fmt.Sprintf("%d/%d", m.picker.Index+1, len(m.picker.Options))
	for _, hint := range selectorHints(width, position) {
		rows = append(rows, styleHints.Render(hint))
	}
	return strings.Join(rows, "\n")
}

func (m *Model) pickerOptionText(index int) (label, description string, disabled bool) {
	if m == nil || m.picker == nil || index < 0 || index >= len(m.picker.Options) {
		return "", "", false
	}
	if index < len(m.picker.Options) {
		option := m.picker.Options[index]
		label, description = sanitizeANSI(option.Label), sanitizeANSI(option.Description)
		disabled = option.Disabled
	}
	if description == "" {
		const separatorText = " — "
		if separator := strings.Index(label, separatorText); separator >= 0 {
			description = strings.TrimSpace(label[separator+len(separatorText):])
			label = strings.TrimSpace(label[:separator])
		}
	}
	return label, description, disabled
}

func selectorHints(width int, position string) []string {
	if width < 34 {
		return []string{"↑↓ move · enter select", "esc cancel · " + position}
	}
	return wrapDisplay("↑↓ move · enter select · esc cancel · "+position, width)
}

func (m *Model) pickerOptionCurrent(index int) bool {
	if m == nil || m.picker == nil || index < 0 || index >= len(m.picker.Options) {
		return false
	}
	option := m.picker.Options[index]
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
	// Paging and rendering share these wrapped rows and the same height budget.
	// Arrow keys select decisions; PgUp/PgDn review the complete source.
	details := m.approvalDetailLines()
	if len(details) == 0 {
		details = []string{"(no additional details)"}
	}
	budget := m.approvalVisibleRows()
	maxOffset := max(0, len(details)-budget)
	offset := min(max(0, m.approvalState.Offset), maxOffset)
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
		rows = append(rows, styleHints.Render(fmt.Sprintf("PgUp/PgDn · %d–%d/%d", offset+1, end, len(details))))
	}
	if tool == "Plan" {
		rows = append(rows, renderApprovalChoices([]string{"Approve plan", "Decline plan"}, m.selectedApprovalChoice())...)
	} else if len(m.approval.Capabilities) > 0 {
		rows = append(rows, renderApprovalChoices([]string{"Allow this call", "Allow for session", "Deny"}, m.selectedApprovalChoice())...)
	} else {
		rows = append(rows, renderApprovalChoices([]string{"Allow once", "Always allow", "Deny"}, m.selectedApprovalChoice())...)
	}
	rows = append(rows, wrapDisplay(styleHints.Render("↑↓ choose · enter confirm · esc later"), width)...)
	return strings.Join(rows, "\n")
}

// renderApprovalChoices draws the ↑↓-selectable approval choices, one per row,
// with the current selection highlighted — mirroring the question/selector
// affordance so the ↑↓ cursor and the rendered list always agree.
func renderApprovalChoices(labels []string, cursor int) []string {
	out := make([]string, len(labels))
	for i, label := range labels {
		if i == cursor {
			out[i] = styleCmdSugSel.Render("❯ " + label)
		} else {
			out[i] = styleHints.Render("  " + label)
		}
	}
	return out
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
	return wrapDisplay("↑↓ choose · Space select · Tab text · Enter next · Esc later", width)
}

func (m *Model) pickerVisibleRows() int {
	if m.picker == nil {
		return 0
	}
	limit := maxPickerRows
	if m.height >= 22 {
		limit = min(24, max(maxPickerRows, m.height/2))
	}
	rows := min(limit, len(m.picker.Options))
	if m.height > 0 {
		overhead := 9
		if m.height >= 22 {
			overhead += 3
		}
		rows = min(rows, max(1, m.height-overhead))
	}
	return rows
}

func (m *Model) pickerWindow() (int, int) {
	visibleRows := m.pickerVisibleRows()
	if visibleRows <= 0 || len(m.picker.Options) == 0 {
		return 0, 0
	}
	m.picker.Selector.SetSize(visibleRows)
	start := m.picker.Selector.Scroll.Offset
	end := min(len(m.picker.Options), start+visibleRows)
	if end-start < visibleRows {
		start = max(0, end-visibleRows)
	}
	return start, end
}

func (m *Model) approvalVisibleRows() int {
	if m.height <= 0 {
		return 6
	}
	width := max(1, m.width-2)
	// Reserve the title, choices, review position, key hints and persistent
	// state first. Remaining rows belong to the actual command, not a hidden
	// composer. Leave two transcript rows when there is room.
	reserved := presentationHeight(m.headerPresentation(), m.width) +
		presentationHeight(m.renderPersistentStatus(), width) + approvalChoiceCount(m.approval) +
		presentationHeight("↑↓ choose · enter confirm · esc later", width) + 2
	if m.approvalPending {
		reserved++
	}
	if m.modalErr != "" {
		reserved++
	}
	return max(1, min(max(8, m.height/2), m.height-reserved-2))
}

func (m *Model) approvalDetailLines() []string {
	if m.approval == nil {
		return nil
	}
	width := max(1, m.width-2)
	command := wrapDisplay(sanitizeANSI(m.approval.Command), width)
	reason := wrapDisplay(sanitizeANSI(m.approval.Reason), width)
	lines := append([]string{}, command...)
	if len(command) > 0 && len(reason) > 0 {
		lines = append(lines, "")
	}
	for _, line := range reason {
		lines = append(lines, styleStatus.Render(line))
	}
	for _, capability := range m.approval.Capabilities {
		lines = append(lines, styleStatus.Render("Sandbox: "+formatApprovalCapability(capability)))
	}
	return lines
}

func formatApprovalCapability(cap protocol.CapabilityRequest) string {
	switch cap.Kind {
	case "directory":
		return fmt.Sprintf("%s directory %s", cap.Access, cap.Path)
	case "network":
		return "outbound TCP/UDP to non-local network destinations (Unix sockets remain blocked)"
	case "local_service":
		return fmt.Sprintf("%s %s localhost service on port %d (Seatbelt may match other host-local addresses on that port)", cap.Direction, cap.Protocol, cap.Port)
	default:
		return "unknown capability request"
	}
}
