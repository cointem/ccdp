package tui

import (
	"context"

	"fmt"

	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

// Live events use numeric turn IDs; the durable journal prefixes them.
func canonicalUITurn(id protocol.TurnID) string {
	s := id.String()
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return "turn-" + s
	}
	return s
}

func (m *Model) hasTurnError(turnID protocol.TurnID, text string) bool {
	if turnID == "" {
		return false
	}
	for _, item := range m.items {
		if item.kind == "error" && (text == "" || item.text == text) && canonicalUITurn(item.turnID) == canonicalUITurn(turnID) {
			return true
		}
	}
	return false
}

// Replace the live error report with its durable row without printing it a
// second time. Matching both turn and source preserves equal errors in other
// turns and distinct diagnostics from the same turn.
func (m *Model) reconcileErrorReport(item historyCell) {
	if item.kind != "error" || item.turnID == "" {
		return
	}
	kept := m.reports[:0]
	for _, report := range m.reports {
		if report.kind != "error" || report.text != item.text || canonicalUITurn(report.turnID) != canonicalUITurn(item.turnID) {
			kept = append(kept, report)
			continue
		}
		m.inline.ensure()
		oldID, newID := itemIdentity(report, 0), itemIdentity(item, 0)
		if _, printed := m.inline.printed[oldID]; printed {
			m.inline.printed[newID] = struct{}{}
		}
		if offset, exists := m.inline.offsets[inlineLogicalKey(report, 0)]; exists {
			m.inline.offsets[inlineLogicalKey(item, 0)] = offset
		}
	}
	m.reports = kept
}

func (m *Model) addReport(item historyCell) {
	if item.reportID == "" {
		item.reportID = nextUICommandID().String()
	} else {
		// Runtime watch resync can replay an operation completion.  Reports are
		// durable presentation units, so a known identity is emitted exactly
		// once even when the event is observed twice.
		for _, existing := range m.reports {
			if existing.reportID == item.reportID {
				return
			}
		}
	}
	item.reportIndex = len(m.confirmedItems)
	if len(m.confirmedItems) > 0 {
		item.reportAnchor = m.confirmedItems[len(m.confirmedItems)-1].messageID
	}
	m.reports = append(m.reports, item)
	if m.routing != nil && len(m.reports) > 128 {
		m.reports = append([]historyCell(nil), m.reports[len(m.reports)-128:]...)
	}
	m.composeItems()
}

// querySessionClient is intentionally optional: the public SessionClient
// contract stays small for remote/recording adapters, while Agent exposes a
// direct bounded Query implementation.  When this extension is absent,
// runQuery submits the same typed CommandQuery through the normal protocol.
type querySessionClient interface {
	Query(context.Context, protocol.QueryKind) (protocol.QueryReport, error)
}

func (m *Model) runQuery(kind protocol.QueryKind, purpose string) tea.Cmd {
	if m.client == nil {
		m.pushStatus("session client unavailable")
		return nil
	}
	client := m.client
	ctx := m.watchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	sid := protocol.SessionID(m.sessionID)
	commandID := nextUICommandID()
	cmd := protocol.Command{ID: commandID, SessionID: sid, Type: protocol.CommandQuery,
		Query: &protocol.QueryCommand{Kind: kind}}
	if queryClient, ok := client.(querySessionClient); ok {
		m.registerOperation(cmd, purpose)
		m.beginActivity(commandID, "", ActivitySubmitting, "正在提交", purpose)
		m.addNotice(Notice{CommandID: commandID, Severity: NoticeInfo, Text: "submitting…", Sticky: false})
		return func() tea.Msg {
			report, err := queryClient.Query(ctx, kind)
			return queryReportMsg{commandID: commandID, report: report, sessionID: sid, err: err}
		}
	}
	return m.submitCommand(cmd, purpose)
}

func (m *Model) applyQueryReport(msg queryReportMsg) {
	if msg.sessionID != "" && msg.sessionID.String() != m.sessionID {
		return
	}
	if op, ok := m.operation(msg.commandID); ok {
		if msg.err != nil {
			m.updateOperation(msg.commandID, OperationFailed, protocol.Receipt{CommandID: msg.commandID, SessionID: msg.sessionID, Error: &protocol.CommandError{Message: msg.err.Error()}})
			m.finishActivity(msg.commandID, ActivityFailed, msg.err.Error())
		} else {
			m.updateOperation(msg.commandID, OperationApplied, protocol.Receipt{CommandID: msg.commandID, SessionID: msg.sessionID})
			m.finishActivity(msg.commandID, ActivityCompleted, op.Purpose)
		}
		m.clearNotice(msg.commandID)
	}
	if msg.err != nil {
		m.addReport(historyCell{kind: "error", text: "query failed: " + msg.err.Error(), reportID: msg.commandID.String()})
		m.addNotice(Notice{CommandID: msg.commandID, Severity: NoticeError, Text: "query failed: " + msg.err.Error(), Sticky: true})
		return
	}
	text := strings.TrimSpace(msg.report.Text)
	if text == "" {
		text = "(empty query result)"
	}
	// A query is read-only, so a late result is still useful, but make its
	// revision boundary explicit instead of allowing an async report to look
	// like current mutable state.
	if m.hasSnapshot && msg.report.Revision.LogSeq != 0 && msg.report.Revision.LogSeq < m.snapshot.Revision.LogSeq {
		text = fmt.Sprintf("[report from revision %d; current revision %d]\n%s", msg.report.Revision.LogSeq, m.snapshot.Revision.LogSeq, text)
	}
	m.addReport(historyCell{kind: "system", text: text, reportID: msg.commandID.String()})
}
