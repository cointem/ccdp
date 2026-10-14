package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
)

func TestGoalCommandWhileBusyPreservesLiteralObjective(t *testing.T) {
	m := NewWithClient(nil, "/workspace", false)
	t.Cleanup(m.watchCancel)
	m.busy = true
	m.hasSnapshot = true
	m.snapshot = protocol.SessionView{SessionID: protocol.SessionID(m.sessionID), Revision: protocol.Revision{LogSeq: 90}}
	rec := &recordingClient{snapshot: m.snapshot}
	m.client = rec
	runSlash(t, &m, `/goal edit fix Bob's test`)
	if len(rec.submits) != 1 || rec.submits[0].Type != protocol.CommandGoal || rec.submits[0].Goal.Objective != "fix Bob's test" || rec.submits[0].ExpectedRevision != 0 {
		t.Fatalf("commands=%+v", rec.submits)
	}
}

func TestGoalQueryAndStatus(t *testing.T) {
	m := NewWithClient(nil, "/workspace", false)
	t.Cleanup(m.watchCancel)
	m.width = 120
	rec := &recordingClient{}
	m.client = rec
	runSlash(t, &m, "/goal")
	if len(rec.submits) != 1 || rec.submits[0].Query == nil || rec.submits[0].Query.Kind != protocol.QueryGoal {
		t.Fatalf("commands=%+v", rec.submits)
	}
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventGoalChanged, Goal: &protocol.Goal{Objective: "ship", Status: protocol.GoalActive, TokenBudget: 500, TokensUsed: 120}})
	status := sanitizeANSI(m.renderStatus())
	if !strings.Contains(status, "goal active") || !strings.Contains(status, "120/500") {
		t.Fatal(status)
	}
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventGoalChanged})
	if strings.Contains(m.renderStatus(), "goal active") {
		t.Fatal("cleared goal still visible")
	}
}
