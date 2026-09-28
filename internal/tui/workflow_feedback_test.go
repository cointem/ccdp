package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

func TestReviewSavedThoughtsReplaceLiveRowsWithoutReprinting(t *testing.T) {
	m := astraModel(t, 100, 30)
	for i := 0; i < 20; i++ {
		item := protocol.TranscriptItem{ID: fmt.Sprintf("review:live:%d", i), Kind: "thinking", Text: "partial thought", Status: "completed"}
		m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventOperationProgress, Transcript: &item})
		m.inline.mark(projectTranscriptCell(item), i)
		tool := protocol.TranscriptItem{ID: fmt.Sprintf("review:tool:%d", i), Kind: "tool", Tool: "Read", Status: "success"}
		m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventOperationProgress, Transcript: &tool})
	}
	for repeat := 0; repeat < 2; repeat++ {
		for i := 0; i < 20; i++ {
			item := protocol.TranscriptItem{ID: fmt.Sprintf("review:saved:%d", i), PreviousID: fmt.Sprintf("review:live:%d", i), Kind: "thinking", Text: "complete thought", Status: "completed"}
			m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventOperationProgress, Transcript: &item})
			if !m.inline.seen(projectTranscriptCell(item), i) {
				t.Fatal("saved thought would print its header again")
			}
		}
	}
	if len(m.confirmedItems) != 40 {
		t.Fatalf("duplicate thoughts: %d", len(m.confirmedItems))
	}
	for i := 0; i < 20; i++ {
		if m.confirmedItems[2*i].messageID != fmt.Sprintf("review:saved:%d", i) || m.confirmedItems[2*i+1].kind != "tool" {
			t.Fatal("thought order changed")
		}
	}
}

func TestWorkflowClockUsesRuntimeStartAcrossPhasesAndReconnect(t *testing.T) {
	clock := time.Now()
	oldNow := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = oldNow })
	m := astraModel(t, 120, 30)
	m.turnStarted = clock.Add(-5 * time.Minute)
	start := clock.Add(-3 * time.Second)
	view := protocol.SessionView{SessionID: protocol.SessionID(m.sessionID), Busy: true,
		Phase: protocol.PhaseExecutingTools, WorkStartedAt: start, WorkLabel: "正在审查代码"}
	m.applySnapshot(view)
	if got := ansi.Strip(m.renderStatus()); !strings.Contains(got, "正在审查代码 · 3s") {
		t.Fatalf("review inherited previous turn clock: %s", got)
	}
	clock = clock.Add(4 * time.Second)
	view.Phase = protocol.PhasePreparing
	m.applySnapshot(view)
	// The user event arrives after checkpoint preparation. It must not reset
	// the clock or disagree with the runtime's final duration.
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, TurnID: "next"})
	if !m.turnStarted.Equal(start) || !strings.Contains(ansi.Strip(m.renderStatus()), "7s") {
		t.Fatalf("phase/event reset clock: %s", m.renderStatus())
	}
	reconnected := astraModel(t, 120, 30)
	view.SessionID = protocol.SessionID(reconnected.sessionID)
	reconnected.applySnapshot(view)
	if !reconnected.turnStarted.Equal(start) {
		t.Fatal("reconnect lost runtime start")
	}
}

func TestReviewReportSurvivesReceiptAndIdleSnapshots(t *testing.T) {
	for _, status := range []string{"complete", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			m := astraModel(t, 120, 30)
			id := protocol.CommandID("review-feedback")
			m.registerOperation(protocol.Command{ID: id, Type: protocol.CommandRunWorkflow}, "review")
			m.operationReports = map[protocol.CommandID]struct{}{id: {}}
			receipt := protocol.Receipt{CommandID: id, OperationID: protocol.OperationID(id), Status: protocol.ReceiptApplied}
			toolStatus := "success"
			if status != "complete" {
				receipt.Status = protocol.ReceiptRejected
				receipt.Error = &protocol.CommandError{Message: "stopped"}
				toolStatus = "error"
			}
			m.applyReceipt(receipt, "")
			ev := protocol.EventView{Kind: protocol.EventToolResult, Tool: &protocol.ToolView{
				ID: protocol.CallID(id), Name: "review", Status: toolStatus,
				Output: `{"id":"review-feedback","snapshot_id":"snapshot","status":"` + status + `","summary":"review result","findings":[],"duration_ms":7000}`}}
			m.handleProtocolEvent(ev)
			m.handleProtocolEvent(ev) // reconnect replay must not duplicate it
			m.applySnapshot(protocol.SessionView{SessionID: protocol.SessionID(m.sessionID), Phase: protocol.PhaseIdle})
			count := 0
			for _, item := range m.items {
				if item.reportID == string(id) {
					count++
					if !strings.Contains(item.text, "Review ·") || !strings.Contains(item.text, "用时 7s") {
						t.Fatalf("missing review report: %s", item.text)
					}
					if status == "complete" && !strings.Contains(item.text, "未发现明确缺陷") {
						t.Fatal("successful empty review must state its outcome")
					}
				}
			}
			if count != 1 {
				t.Fatalf("visible reports = %d", count)
			}
		})
	}
}

func TestReviewFailureBeforeSnapshotIsNotEmpty(t *testing.T) {
	out := codingReport(`{"id":"review-cancel","snapshot_id":"","status":"cancelled","findings":[],"duration_ms":1200,"summary":"context canceled"}`)
	if !strings.Contains(out, "审查已停止") || !strings.Contains(out, "本次审查尚未执行") {
		t.Fatalf("unreadable early cancellation: %s", out)
	}
}

func TestReviewProcessUsesTranscriptWithoutStartingConversation(t *testing.T) {
	m := astraModel(t, 100, 30)
	m.busy = true
	m.activity = Activity{Phase: ActivityRunningTool, Label: "正在审查"}
	items := []protocol.TranscriptItem{
		{ID: "review:one:stage", Kind: "system", Text: "正在审查 1 个变更文件", Status: "completed"},
		{ID: "review:one:reasoning", Kind: "thinking", Text: "Checking callers", Status: "streaming"},
		{ID: "review:one:tool", Kind: "tool", Tool: "Read", CallID: "review:one:call", Status: "running"},
	}
	for _, item := range items {
		m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventOperationProgress, Transcript: &item})
	}
	items[1].Status = "completed"
	items[2].Status, items[2].Text = "success", "package p"
	for _, item := range items {
		m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventOperationProgress, Transcript: &item})
	}
	if m.streaming || !m.busy || m.activity.Label != "正在审查" {
		t.Fatal("review progress took ownership of conversation lifecycle")
	}
	if len(m.confirmedItems) != 3 || m.confirmedItems[2].text != "package p" || m.confirmedItems[2].status != "success" {
		t.Fatalf("review progress did not update ordinary transcript rows: %+v", m.confirmedItems)
	}
}
