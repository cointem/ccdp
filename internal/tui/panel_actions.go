package tui

import (
	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"strconv"
	"strings"
)

// handleApprovalKey resolves an approval modal.
func (m *Model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.approval == nil {
		return m, nil
	}
	if msg.String() == "esc" && m.routing != nil && m.sessionID != m.routing.rootID {
		return m, m.openAgentView(m.parentAgentID())
	}
	// Keep the details scrollable while a decision is in flight, but never
	// submit a second decision for the same modal before its receipt.
	if m.approvalPending {
		switch msg.String() {
		case "enter", "up", "down", "j", "k":
			return m, nil
		}
	}
	var approve, remember bool
	var capabilityScope protocol.CapabilityScope
	handled := true
	switch msg.String() {
	case "ctrl+c":
		return m, m.requestInterrupt()
	case "ctrl+o":
		return m, m.openDecisionReader()
	case "up", "k":
		m.selectApprovalChoice(-1)
		return m, nil
	case "down", "j":
		m.selectApprovalChoice(1)
		return m, nil
	case "pgup", "ctrl+u":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.PageUp()
		return m, nil
	case "pgdown", "ctrl+d":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.PageDown()
		return m, nil
	case "home":
		m.approvalState.Home()
		return m, nil
	case "end":
		m.approvalState.Set(m.approvalVisibleRows(), len(m.approvalDetailLines()))
		m.approvalState.End()
		return m, nil
	case "enter":
		if m.selectedApprovalChoice() < 0 {
			m.modalErr = "Choose with ↑↓ before confirming."
			return m, nil
		}
		m.approvalCursor = m.selectedApprovalChoice()
		if m.approval.Tool == "Plan" {
			approve = m.approvalCursor == 0
		} else if len(m.approval.Capabilities) > 0 {
			switch m.approvalCursor {
			case 1:
				approve, capabilityScope = true, protocol.CapabilityScopeSession
			case 2:
				approve = false
			default:
				approve, capabilityScope = true, protocol.CapabilityScopeOnce
			}
		} else {
			switch m.approvalCursor {
			case 1:
				approve, remember = true, true
			case 2:
				approve, remember = false, false
			default:
				approve, remember = true, false
			}
		}
	case "esc":
		m.deferredApproval = m.approvalKey()
		m.modalErr = ""
		return m, nil
	default:
		handled = false
	}
	if handled {
		if m.watchDisconnected {
			m.modalErr = "Disconnected: reopen after /reconnect. Decision not submitted."
			return m, nil
		}
		approval := *m.approval
		m.pendingApprovalKey = m.approvalKey()
		if approval.Tool == "Plan" {
			cmd := protocol.Command{Type: protocol.CommandApprovePlan,
				Plan: &protocol.ApprovePlan{PlanID: approval.ID, Approve: approve}}
			if m.snapshot.Plan != nil {
				cmd.Plan.PlanVersion = m.snapshot.Plan.Version
			}
			m.approvalPending = true
			m.pendingApprovalCommand = cmd.ID
			if cmd.ID == "" {
				cmd.ID = nextUICommandID()
				m.pendingApprovalCommand = cmd.ID
			}
			return m, m.submitCommand(cmd, "plan decision submitted")
		}
		cmd := protocol.Command{Type: protocol.CommandApproveTool,
			Approval: &protocol.ApproveTool{ApprovalID: approval.ID, Approve: approve, Remember: remember, CapabilityScope: capabilityScope}}
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

// approvalChoiceCount reports how many ↑↓ choices the approval modal offers:
// 2 for a Plan approval (approve/deny), 3 for a tool approval (allow once /
// always allow / deny).
func approvalChoiceCount(req *approvalPrompt) int {
	if req != nil && req.Tool == "Plan" {
		return 2
	}
	return 3
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
			step = max(1, m.pickerVisibleRows())
		}
		m.picker.Index -= step
		if m.picker.Index < 0 {
			m.picker.Index = 0
		}
	case "down", "j", "fn+down", "alt+down", "shift+down", "mousewheeldown", "wheeldown":
		m.picker.buf = ""
		step := 1
		if strings.HasPrefix(msg.String(), "fn+") || strings.HasPrefix(msg.String(), "alt+") || strings.HasPrefix(msg.String(), "shift+") {
			step = max(1, m.pickerVisibleRows())
		}
		m.picker.Index = min(len(m.picker.Options)-1, m.picker.Index+step)
	case "pgup", "ctrl+u":
		m.picker.buf = ""
		m.picker.Index = max(0, m.picker.Index-max(1, m.pickerVisibleRows()))
	case "pgdown", "ctrl+d":
		m.picker.buf = ""
		m.picker.Index = min(len(m.picker.Options)-1, m.picker.Index+max(1, m.pickerVisibleRows()))
	case "home":
		m.picker.buf = ""
		m.picker.Index = 0
	case "end":
		m.picker.buf = ""
		m.picker.Index = len(m.picker.Options) - 1
	case "enter":
		selection := m.picker.Index
		if m.picker.buf != "" {
			n := 0
			for _, r := range m.picker.buf {
				n = n*10 + int(r-'0')
			}
			selection = n - 1
		}
		if selection < 0 || selection >= len(m.picker.Options) {
			m.modalErr = "enter a number between 1 and " + strconv.Itoa(len(m.picker.Options))
			m.picker.buf = ""
			return m, nil
		}
		// A disabled option is an explanation surface, not an executable
		// action. Keep the picker open so the user can read its reason, move to
		// an available option, or cancel without accidentally submitting a
		// mutation.
		if selection < len(m.picker.Options) && m.picker.Options[selection].Disabled {
			reason := strings.TrimSpace(m.picker.Options[selection].Description)
			if reason == "" {
				reason = "option unavailable"
			}
			m.modalErr = reason
			m.picker.buf = ""
			m.picker.SetSize(m.pickerVisibleRows())
			return m, nil
		}
		p := *m.picker
		m.picker = nil
		m.syncViewportHeight()
		if selection < len(p.Options) {
			m.mouseReleasePending = true
			cmd := m.executeSelectorAction(p.action, p.Options[selection].ID)
			if cmd == nil {
				m.mouseReleasePending = false
				return m, restoreMouseCmd()
			}
			return m, cmd
		}
		return m, restoreMouseCmd()
	case "esc", "ctrl+c":
		m.picker = nil
		m.syncViewportHeight()
		return m, restoreMouseCmd()
	default:
		if msg.Type == tea.KeyRunes {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, msg.String())
			if digits != "" && len(m.picker.buf) < len(strconv.Itoa(len(m.picker.Options))) {
				m.picker.buf += digits
				if n, err := strconv.Atoi(m.picker.buf); err == nil && n >= 1 && n <= len(m.picker.Options) {
					m.picker.Index = n - 1
				}
			}
		}
	}
	m.picker.SetSize(m.pickerVisibleRows())
	return m, nil
}

func (m *Model) startSelectorAt(title string, options []SelectorOption, selected int, inline bool, action selectorAction) tea.Cmd {
	if len(options) == 0 {
		m.pushStatus(title + ": no items")
		return nil
	}
	selected = min(max(0, selected), len(options)-1)
	m.picker = &selectorPanel{Selector: Selector{Title: title, Options: append([]SelectorOption(nil), options...), Index: selected}, action: action, inline: inline}
	m.picker.SetSize(m.pickerVisibleRows())
	m.syncViewportHeight()
	m.modalErr = ""
	// Ordinary-screen coordinates are relative to the shell cursor, not the
	// terminal origin. Keep selectors keyboard-driven and native selection on.
	return nil
}
