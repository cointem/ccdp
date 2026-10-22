package tui

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/sessionview"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReaderPageCompletesInCachedSession(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	navigate := m.openAgentView("child")
	m.readerRequest = 1
	m.readerSeq = 1
	{
		next, _ := m.Update(agentOutputMsg{sessionID: "root", itemID: "tool", request: 1, page: protocol.OutputPage{Text: "page1", Next: 16, Total: 32, More: true}})
		m = modelValue(t, next)
	}
	load := m.loadNextReaderPage()
	if load == nil {
		t.Fatal("no load")
	}
	next, _ := m.Update(navigate())
	m = modelValue(t, next)
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	m = modelValue(t, next)
	next, _ = m.Update(load())
	m = modelValue(t, next)
	m = switchRoutingTest(t, m, "root")
	if m.reader == nil || m.reader.loading || m.reader.sourceText() != "saved output" || !m.reader.screenEntered {
		t.Fatalf("cached reader did not receive its page and regain the alternate screen: %+v", m.reader)
	}
}
func TestReaderClearedOnResume(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-session", true: "same-session"}[sameSession], func(t *testing.T) {
			m, _ := routingTestModel()
			source := newLifecycleTestAgent(t)
			defer source.Close()
			candidate := newLifecycleTestAgent(t)
			defer candidate.Close()
			m.ag = source
			snapshot, err := candidate.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if sameSession {
				m.sessionID = snapshot.SessionID.String()
			}
			m.confirmedItems = []historyCell{{kind: "assistant", text: "OLD SESSION SECRET", messageID: "old"}}
			m.openTranscriptReader()
			next, _ := m.handleResumeOpened(resumeOpenedMsg{source: source, candidate: candidate, snapshot: snapshot, expectedSessionID: m.sessionID, expectedGeneration: m.watchGeneration})
			m = modelValue(t, next)
			defer m.Close()
			if m.reader != nil {
				t.Fatalf("new session %s still displays old reader", m.sessionID)
			}
		})
	}
}

func TestReaderHistoryCompletionWhileCached(t *testing.T) {
	for _, outcome := range []string{"success", "empty", "error"} {
		t.Run(outcome, func(t *testing.T) {
			m, _ := routingTestModel()
			defer m.Close()
			navigate := m.openAgentView("child")
			m.openTranscriptReader()
			// Ensure an existing reader even when the fixture has no transcript.
			m.reader = &readerState{kind: readerTranscript, sessionID: "root", loading: true}
			load := m.loadAgentHistory(0)
			msg := load().(agentHistoryMsg)
			switch outcome {
			case "success":
				msg.page.Items = []protocol.TranscriptItem{{ID: "saved", Kind: "assistant", Text: "history page"}}
			case "error":
				msg.err = errors.New("history unavailable")
			}
			next, _ := m.Update(navigate())
			m = modelValue(t, next)
			next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
			m = modelValue(t, next)
			next, cmd := m.Update(msg)
			m = modelValue(t, next)
			if cmd != nil || m.reader != nil {
				t.Fatal("cached reader callback operated the child's terminal")
			}
			m = switchRoutingTest(t, m, "root")
			if m.reader == nil || m.reader.loading {
				t.Fatal("history reader remained loading")
			}
			if outcome == "success" && m.reader.sourceText() != "history page" {
				t.Fatal("history result was lost")
			}
			if outcome == "error" && !strings.Contains(m.reader.errText, "unavailable") {
				t.Fatal("history failure was lost")
			}
		})
	}
}

func TestReaderOutputFailureWhileCached(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	navigate := m.openAgentView("child")
	{
		next, _ := m.Update(agentOutputMsg{sessionID: "root", itemID: "tool",
			page: protocol.OutputPage{Text: "first", Next: 10, More: true}})
		m = modelValue(t, next)
	}
	msg := m.loadNextReaderPage()().(agentOutputMsg)
	msg.err = errors.New("page unavailable")
	next, _ := m.Update(navigate())
	m = modelValue(t, next)
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	m = modelValue(t, next)
	next, _ = m.Update(msg)
	m = modelValue(t, next)
	m = switchRoutingTest(t, m, "root")
	if m.reader == nil || m.reader.loading || m.reader.sourceText() != "first" || m.loadNextReaderPage() == nil {
		t.Fatal("failed cached page did not retain its content and allow retry")
	}
}

func TestReaderRejectsClosedRequestAndPreviousAttachment(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	msg := m.loadAgentHistory(0)().(agentHistoryMsg)
	msg.page.Items = []protocol.TranscriptItem{{ID: "old", Kind: "assistant", Text: "old page"}}
	m.reader = &readerState{}
	m.closeReader()
	next, _ := m.Update(msg)
	m = modelValue(t, next)
	if m.reader != nil {
		t.Fatal("closed reader reopened from a late request")
	}
	// Even an equal request number in the same session cannot cross handles.
	m.readerRequest = msg.request
	m.attachmentGeneration++
	next, _ = m.Update(msg)
	m = modelValue(t, next)
	if m.reader != nil {
		t.Fatal("previous attachment reopened its reader")
	}
}

func TestOperationReportReceiptOrderingAndRecovery(t *testing.T) {
	for _, order := range []string{"receipt-first", "result-first", "recovered"} {
		t.Run(order, func(t *testing.T) {
			m, d := routingTestModel()
			defer m.Close()
			const id protocol.CommandID = "apply-report"
			const output = "patch failed\nerror: a.go: patch does not apply"
			m.client = &recoveryClient{recordingClient: d.clients["root"],
				result: protocol.OperationResult{ID: id, Complete: true, Status: "error", Output: output, Error: "git apply exited with status 1"}}
			m.submitCommand(protocol.Command{ID: id, Type: protocol.CommandApply}, "apply")
			receipt := protocol.Receipt{CommandID: id, SessionID: "root", OperationID: protocol.OperationID(id),
				Status: protocol.ReceiptRejected, Error: &protocol.CommandError{Code: protocol.ErrorInternal, Message: "git apply exited with status 1"}}
			event := protocol.EventView{SessionID: "root", Kind: protocol.EventToolResult,
				Tool: &protocol.ToolView{ID: protocol.CallID(id), Name: "apply", Status: "error", Output: output}}
			if order == "result-first" {
				m.handleProtocolEvent(event)
			}
			m.applyReceipt(receipt, "")
			if order == "recovered" {
				m = recoverTestOperations(t, m)
			}
			m.handleProtocolEvent(event)
			m.applyReceipt(receipt, "") // delayed duplicate receipt
			count := 0
			for _, report := range m.reports {
				if report.reportID == id.String() {
					count++
					if report.kind != "error" || report.text != output {
						t.Fatalf("report=%+v", report)
					}
				}
			}
			if count != 1 || len(m.operationReports) != 0 {
				t.Fatalf("report count=%d waiting=%v", count, m.operationReports)
			}
		})
	}
	m, _ := routingTestModel()
	defer m.Close()
	m.submitCommand(protocol.Command{ID: "denied", Type: protocol.CommandApply}, "apply")
	m.applyReceipt(protocol.Receipt{CommandID: "denied", SessionID: "root", Status: protocol.ReceiptRejected}, "")
	if len(m.operationReports) != 0 {
		t.Fatal("admission rejection retained a nonexistent report")
	}
}

func TestForkReportsCreatedSessionID(t *testing.T) {
	a := newLifecycleTestAgent(t)
	defer a.Close()
	m := NewWithResume(a, false)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sub, err := a.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	m.subscription = sub
	initial := <-sub.Updates()
	next, _ := m.Update(watchUpdateMsg{sessionID: protocol.SessionID(m.sessionID), generation: m.watchGeneration, sub: sub, update: initial})
	m = modelValue(t, next)
	_, submit := m.runCommand("/fork")
	if submit == nil {
		t.Fatal("fork unavailable")
	}
	next, _ = m.Update(submit())
	m = modelValue(t, next)
	for {
		select {
		case u := <-sub.Updates():
			next, _ = m.Update(watchUpdateMsg{sessionID: protocol.SessionID(m.sessionID), generation: m.watchGeneration, sub: sub, update: u})
			m = modelValue(t, next)
			if u.Event != nil && u.Event.Tool != nil && u.Event.Tool.Name == "fork" {
				id := strings.TrimPrefix(u.Event.Tool.Output, "forked session ")
				if id == u.Event.Tool.Output || id == "" {
					t.Fatalf("bad fork output %q", u.Event.Tool.Output)
				}
				for _, cell := range m.items {
					if strings.Contains(cell.text, id) {
						return
					}
				}
				t.Fatalf("fork succeeded as %s but new session ID is absent from every UI cell; reports=%+v", id, m.reports)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestRecoveredOrdinaryToolAllowsFutureScrollback(t *testing.T) {
	call := session.ToolCall{CallID: "old-call", ToolID: "Read", Arguments: []byte(`{"file_path":"a.txt"}`)}
	tx := sessionview.FromRecords([]session.Record{
		{Event: session.AssistantCommitted{TurnID: "old-turn", StepID: "old-step", Message: session.Message{MessageID: "old-message", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentToolCall, ToolCall: &call}}}}},
		{Event: session.ToolStarted{TurnID: "old-turn", StepID: "old-step", Call: call}},
	})
	tx.Facts([]session.Event{session.AssistantCommitted{TurnID: "new-turn", StepID: "new-step", Message: session.Message{MessageID: "new-message", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "NEW TURN COMPLETED"}}}}})
	items, _ := tx.Snapshot()
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.applySnapshot(protocol.SessionView{SessionID: "session", Phase: protocol.PhaseIdle, Transcript: items})
	if cmd := m.planHistory(); cmd == nil {
		t.Fatalf("recovered %s %s blocks future completed assistant from scrollback: %+v", items[0].Tool, items[0].Status, items)
	}
}
func TestRejectedScheduledCommandKeepsFullReport(t *testing.T) {
	m, client := routingTestModel()
	defer m.Close()
	_ = client
	const id protocol.CommandID = "failed-git"
	cmd := protocol.Command{ID: id, SessionID: "root", Type: protocol.CommandExternal, External: &protocol.ExternalCommand{Program: "git", Args: []string{"merge", "branch"}}}
	if m.submitCommand(cmd, "/git merge branch") == nil {
		t.Fatal("submit unavailable")
	}
	m.applyReceipt(protocol.Receipt{CommandID: id, SessionID: "root", Status: protocol.ReceiptScheduled, OperationID: protocol.OperationID(id)}, "")
	// Production settleOperation publishes terminal Receipt, then ToolResult, then Error.
	reason := "git exited with status 1: Auto-merging a.go"
	m.applyReceipt(protocol.Receipt{CommandID: id, SessionID: "root", Status: protocol.ReceiptRejected, OperationID: protocol.OperationID(id), Error: &protocol.CommandError{Code: protocol.ErrorInternal, Message: reason}}, "")
	const detail = "Auto-merging a.go\nCONFLICT (content): Merge conflict in a.go\nAutomatic merge failed; fix conflicts and then commit the result."
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventToolResult, Tool: &protocol.ToolView{ID: protocol.CallID(id), Name: "git", Status: "error", Output: detail}})
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventError, Error: reason})
	for _, c := range m.items {
		if strings.Contains(c.text, "CONFLICT (content)") {
			return
		}
	}
	t.Fatalf("failed git's full report disappeared; only first-line error remains: %+v", m.reports)
}
