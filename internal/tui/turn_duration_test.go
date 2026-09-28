package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

func TestTurnDurationRendersBelowFinalAnswer(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.applyTranscript([]protocol.TranscriptItem{
		{ID: "answer-1", Kind: "assistant", TurnID: "turn-1", Text: "The work is done.", Status: "completed"},
		{ID: "turn-summary:turn-1", Kind: "turn_summary", TurnID: "turn-1", Status: "success", DurationMs: 155_000},
	})
	m.turnDone = true
	m.layout()
	frame := ansi.Strip(m.View())
	answer := strings.Index(frame, "The work is done.")
	duration := strings.Index(frame, "· 运行结束 · 用时 2m 35s")
	if answer < 0 || duration <= answer {
		t.Fatalf("turn duration was not shown below the final answer:\n%s", frame)
	}
	if transcriptGap("assistant", "turn_summary") {
		t.Fatal("turn duration should sit directly below the final answer")
	}
}

func TestTurnDurationShowsFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct{ outcome, label string }{
		{"error", "失败"},
		{"cancelled", "已停止"},
	} {
		cell := projectTranscriptCell(protocol.TranscriptItem{Kind: "turn_summary", Status: tc.outcome, DurationMs: 4_000})
		if !strings.Contains(ansi.Strip(renderItemWidth(&cell, 80)), tc.label+" · 用时 4s") {
			t.Fatalf("%s duration cell = %q", tc.outcome, cell.text)
		}
	}
}

func TestCompletedStreamTransitionsToFinalizingThenIdle(t *testing.T) {
	m := astraModel(t, 100, 24)
	view := protocol.SessionView{SessionID: protocol.SessionID(m.sessionID), Busy: true, Phase: protocol.PhaseStreaming}
	view.Transcript = []protocol.TranscriptItem{{ID: "answer", Kind: "assistant", Text: "hello", Status: "streaming"}}
	m.applySnapshot(view)
	if !m.streaming {
		t.Fatal("streaming snapshot did not set output state")
	}
	view.Phase = protocol.PhaseFinalizing
	view.Transcript = []protocol.TranscriptItem{{ID: "answer", Kind: "assistant", Text: "done", Status: "completed"}}
	m.applySnapshot(view)
	status := ansi.Strip(m.renderStatus())
	if m.streaming || strings.Contains(status, "正在输出") || !strings.Contains(status, "正在保存本轮结果") {
		t.Fatalf("completed response still looks like streaming: %s", status)
	}
	// Resync may deliver only the idle snapshot, without a TurnDone event.
	view.Busy, view.Phase = false, protocol.PhaseIdle
	view.LastTurn = &protocol.TurnOutcome{TurnID: "1", Status: protocol.TurnSucceeded}
	m.applySnapshot(view)
	if m.streaming || m.busy || m.activity.Active() || strings.Contains(ansi.Strip(m.renderStatus()), "正在") {
		t.Fatalf("idle snapshot left a running status: %s", m.renderStatus())
	}
}
