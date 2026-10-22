package tui

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
)

// Pending input previews are composer chrome, never transcript cells or
// transient notices. Local operations cover the gap before admission appears
// in a snapshot; the runtime queue also covers reconnects and other clients.
func (m Model) visiblePendingInputs() []protocol.InputView {
	var inputs []protocol.InputView
	seen := make(map[protocol.InputID]bool)
	delivered := make(map[protocol.InputID]bool)
	for _, op := range m.operations {
		if op.Type == protocol.CommandSubmitInput && op.TurnID != "" {
			delivered[op.InputID] = true
		}
	}
	appendInput := func(input protocol.InputView) {
		if input.Strategy == protocol.InputMessage || seen[input.ID] || delivered[input.ID] {
			return
		}
		// A user event may precede the state snapshot that removes this input.
		if pos, ok := m.positions[protocol.InputMessageID(input.ID)]; ok && m.confirmedItems[pos.confirmed].turnID != "" {
			return
		}
		seen[input.ID] = true
		inputs = append(inputs, input)
	}
	for _, input := range m.pendingInputs {
		appendInput(input)
	}
	for _, op := range m.activeInputOperations() {
		if op.TurnID != "" || !op.Recoverable || op.InputCommand == nil || op.SessionID.String() != m.sessionID {
			continue
		}
		input := op.InputCommand.Input
		text := input.Text
		if len(input.Images) > 0 {
			text += fmt.Sprintf(" [%d images]", len(input.Images))
		}
		appendInput(protocol.InputView{ID: op.InputID, Text: text, Strategy: input.Strategy})
	}
	return inputs
}

func (m Model) renderPendingInputs() string {
	inputs := m.visiblePendingInputs()
	if len(inputs) == 0 {
		return ""
	}
	// Bound the shelf so long drafts and multiple submissions cannot displace
	// the composer on small terminals. Full text stays in the queue/history.
	limit := min(3, max(1, m.height/6))
	rows := make([]string, 0, limit)
	for i, input := range inputs {
		if i >= limit {
			break
		}
		label := "queued"
		if input.Strategy == protocol.InputFollowup {
			label = "follow_up"
		}
		if i == limit-1 && len(inputs) > limit {
			label += fmt.Sprintf(" (+%d)", len(inputs)-limit)
		}
		text := strings.Join(strings.Fields(sanitizeANSI(input.Text)), " ")
		rows = append(rows, " "+styleHints.Render(truncateDisplay(label+" · "+text, max(1, m.width-2))))
	}
	return strings.Join(rows, "\n")
}
