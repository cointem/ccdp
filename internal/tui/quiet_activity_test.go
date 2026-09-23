package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
	"time"
)

func TestQuietExecutionIsNotUnresponsive(t *testing.T) {
	for _, phase := range []ActivityPhase{ActivityRunningTool, ActivityWaitingApproval, ActivityWaitingQuestion, ActivityCompacting, ActivityStopping} {
		m := astraModel(t, 100, 24)
		m.busy = true
		m.activity = Activity{Phase: phase, UpdatedAt: now().Add(-time.Minute)}
		if m.turnStalled() || strings.Contains(m.renderStatus(), "无响应") {
			t.Fatalf("normal quiet phase mislabeled: %s", phase)
		}
	}
	m := astraModel(t, 100, 24)
	m.busy = true
	m.activity = Activity{Phase: ActivityStreaming, UpdatedAt: now().Add(-time.Minute)}
	m.items = []historyCell{{kind: "tool", toolName: "Task", status: "running"}}
	if m.turnStalled() {
		t.Fatal("active tool mislabeled")
	}
	m.items = nil
	m.routing = &sessionRouting{rootID: m.sessionID, rows: []protocol.ChildSession{{ParentSessionID: protocol.SessionID(m.sessionID), Run: protocol.RunView{Status: "running"}}}}
	if m.turnStalled() {
		t.Fatal("active child mislabeled")
	}
	m.routing = nil
	if got := m.renderStatus(); !strings.Contains(got, "等待模型响应") || strings.Contains(got, "无响应") {
		t.Fatalf("wrong model silence hint: %s", got)
	}
	m.activity.UpdatedAt = now().Add(-7 * time.Second)
	if m.turnStalled() {
		t.Fatal("normal first-token delay mislabeled")
	}
}

func TestUnchangedSnapshotDoesNotResetModelSilence(t *testing.T) {
	m := astraModel(t, 100, 24)
	s := m.snapshot
	s.Busy = true
	s.Phase = protocol.PhaseStreaming
	s.Transcript = []protocol.TranscriptItem{{ID: "thought", Kind: "thinking", Status: "streaming", Text: "partial"}}
	m.applySnapshot(s)
	past := now().Add(-time.Minute)
	m.activity.UpdatedAt = past
	m.applySnapshot(s)
	if !m.activity.UpdatedAt.Equal(past) || !m.turnStalled() {
		t.Fatal("unchanged snapshot hid model silence")
	}
	s.Transcript = append([]protocol.TranscriptItem(nil), s.Transcript...)
	s.Transcript[0].Text += " new output"
	m.applySnapshot(s)
	if m.turnStalled() {
		t.Fatal("new output did not reset silence")
	}
}
