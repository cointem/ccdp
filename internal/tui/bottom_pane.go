package tui

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type panelKind uint8

const (
	panelNone panelKind = iota
	panelDetail
	panelTasks
	panelExit
	panelReader
	panelSelector
	panelQuestion
	panelApproval
	panelHistorySearch
	panelContext
	panelCommands
	panelFiles
)

func (m *Model) activePanel() panelKind {
	switch {
	case m.exitConfirm:
		return panelExit
	case m.reader != nil:
		return panelReader
	case m.detail != nil:
		return panelDetail
	case m.picker != nil:
		return panelSelector
	case m.questionActive():
		return panelQuestion
	case m.approvalActive():
		return panelApproval
	case m.histSearch != nil:
		return panelHistorySearch
	case m.showContextDetail:
		return panelContext
	case len(m.cmdSug) > 0:
		return panelCommands
	case m.fileMention != nil:
		return panelFiles
	case m.tasksVisible && len(m.snapshot.Tasks) > 0:
		return panelTasks
	default:
		return panelNone
	}
}

// Decisions remain in the session projection when dismissed. Only the local
// focus changes; a dismissal must never send a runtime approval or rejection.
func (m *Model) approvalKey() string {
	if m.approval == nil {
		return ""
	}
	key := m.sessionID + "\x00" + m.approval.ID + "\x00" + m.approval.Tool + "\x00" + m.approval.Command + "\x00" + m.approval.Reason
	if m.approval.Tool == "Plan" && m.snapshot.Plan != nil {
		key += fmt.Sprintf("\x00%d", m.snapshot.Plan.Version)
	}
	return key
}

func (m *Model) approvalActive() bool {
	return m.approval != nil && m.deferredApproval != m.approvalKey()
}

func (m *Model) questionActive() bool {
	return m.question != nil && !m.question.deferred
}

func (m *Model) composerVisible() bool {
	switch m.activePanel() {
	case panelExit, panelReader, panelSelector, panelQuestion, panelApproval, panelHistorySearch:
		return false
	default:
		return true
	}
}

func (m *Model) selectedApprovalChoice() int {
	if m.approval == nil {
		return -1
	}
	if m.approvalSelection != m.approvalKey() || !m.approvalSelected {
		return 0
	}
	return m.approvalCursor
}

func (m *Model) selectApprovalChoice(delta int) {
	count := approvalChoiceCount(m.approval)
	m.approvalCursor = min(count-1, max(0, m.selectedApprovalChoice()+delta))
	m.approvalSelection, m.approvalSelected = m.approvalKey(), true
	m.modalErr = ""
}

// bottomSurface is the single focus-owning interaction region. Its measurement
// and rendering share the same function so resizing cannot steal review rows.
func (m *Model) bottomSurface() string {
	switch m.activePanel() {
	case panelDetail:
		return m.renderDetailPopover()
	case panelTasks:
		return m.renderTasks()
	case panelExit:
		return strings.Join([]string{"Exit ccdp? Running work will be stopped.",
			"Press y to save and exit; Esc to keep working."}, "\n")
	case panelSelector:
		return m.renderInlineSurface()
	case panelQuestion:
		return m.renderQuestionInline()
	case panelApproval:
		return m.renderApprovalInline()
	case panelHistorySearch:
		return m.renderHistorySearch()
	case panelContext:
		return m.renderContextDetailPopover()
	case panelCommands:
		return m.renderCmdSuggest()
	case panelFiles:
		return m.renderFileMention()
	default:
		return ""
	}
}

func (m *Model) pendingDecisionSummary() string {
	if m.watchDisconnected {
		return fmt.Sprintf("! disconnected · retry %d/5 · /reconnect", m.watchRetry)
	}
	n := 0
	if m.approval != nil {
		n++
	}
	if m.question != nil {
		n++
	}
	if m.routing != nil {
		for _, child := range m.routing.rows {
			if childAwaitingApproval(child) && string(child.SessionID) != m.sessionID {
				n++
			}
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("◇ %d pending · /pending", n)
}

func (m *Model) openPendingDecision() tea.Cmd {
	if m.question != nil {
		m.question.deferred = false
		return nil
	}
	if m.approval != nil {
		m.deferredApproval = ""
		m.approvalSelected = false
		return nil
	}
	if child := m.pendingChildApproval(); child != nil {
		return m.openAgentView(string(child.SessionID))
	}
	m.pushStatus("no pending decisions")
	return nil
}

func (m *Model) requestInterrupt() tea.Cmd {
	if m.watchDisconnected {
		m.pushStatus("disconnected · /reconnect before interrupting")
		return nil
	}
	if m.interruptRequested {
		return nil
	}
	m.interruptRequested = true
	m.pushStatus("interrupting agent…")
	return m.submitCommand(protocol.Command{Type: protocol.CommandInterrupt}, "interrupt requested")
}

func (m *Model) requestQuit() tea.Cmd {
	active := m.busy
	if m.routing != nil {
		for _, child := range m.routing.rows {
			active = active || child.Run.Active()
		}
	}
	if active {
		m.exitConfirm = true
		return nil
	}
	return tea.Quit
}

func (m *Model) openDecisionReader() tea.Cmd {
	if m.approval == nil {
		return nil
	}
	m.readerSeq++
	m.readerRequest = m.readerSeq
	text := m.approval.Command + "\n\n" + m.approval.Reason
	m.reader = &readerState{kind: readerOutput, title: "Review " + m.approval.Tool,
		sessionID: protocol.SessionID(m.sessionID), text: text, rawText: text,
		total: int64(len(text)), request: m.readerRequest,
		returnOffset: m.viewport.YOffset, returnFollow: m.followOutput}
	return m.enterReaderScreen()
}

// Transient panels overlay the base frame; none reserve transcript rows.
func (m *Model) overlaySurface() bool {
	panel := m.activePanel()
	return panel != panelNone && panel != panelReader
}
