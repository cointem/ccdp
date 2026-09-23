package tui

import (
	"bytes"
	"ccdp/internal/protocol"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testSessionDirectory struct {
	clients  map[protocol.SessionID]*recordingClient
	rows     []protocol.ChildSession
	controls []protocol.AgentControl
}

func (d *testSessionDirectory) ListChildren(context.Context) ([]protocol.ChildSession, error) {
	return d.rows, nil
}
func (d *testSessionDirectory) OpenReader(_ context.Context, id protocol.SessionID) (protocol.SessionClient, error) {
	if c := d.clients[id]; c != nil {
		return c, nil
	}
	return nil, errors.New("missing session")
}
func (*testSessionDirectory) ReadTranscript(context.Context, protocol.SessionID, uint64, int) (protocol.TranscriptPage, error) {
	return protocol.TranscriptPage{}, nil
}
func (*testSessionDirectory) ReadOutput(context.Context, protocol.SessionID, string, int64, int) (protocol.OutputPage, error) {
	return protocol.OutputPage{Text: "saved output"}, nil
}
func (d *testSessionDirectory) Control(_ context.Context, c protocol.AgentControl) (protocol.RunView, error) {
	d.controls = append(d.controls, c)
	return protocol.RunView{}, nil
}

func routingTestModel() (Model, *testSessionDirectory) {
	root := &recordingClient{snapshot: protocolSnapshot("root")}
	child := &recordingClient{snapshot: protocolSnapshot("child")}
	child.snapshot.RunID = "run-child"
	d := &testSessionDirectory{clients: map[protocol.SessionID]*recordingClient{"root": root, "child": child}, rows: []protocol.ChildSession{{SessionID: "child", ParentSessionID: "root", Run: protocol.RunView{ID: "run-child", Status: "running"}}}}
	m := NewWithClient(root, "workspace", false)
	m.applySnapshot(root.snapshot)
	m.installSessionRouting(d)
	m.routing.rows = d.rows
	m.width, m.height = 100, 30
	m.layout()
	return m, d
}

func switchRoutingTest(t *testing.T, m Model, id string) Model {
	t.Helper()
	cmd := m.openAgentView(id)
	if cmd == nil {
		t.Fatal("no open command")
	}
	next, _ := m.Update(cmd())
	m = modelValue(t, next)
	if !m.routing.switching {
		t.Fatal("missing terminal barrier")
	}
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	return modelValue(t, next)
}

func TestAgentNavigationPreservesDraftsAndDoesNotSubmitControl(t *testing.T) {
	m, d := routingTestModel()
	m.textarea.SetValue("main draft")
	m = switchRoutingTest(t, m, "child")
	if m.sessionID != "child" {
		t.Fatal("wrong target")
	}
	m.textarea.SetValue("child draft")
	m = switchRoutingTest(t, m, "root")
	if m.textarea.Value() != "main draft" {
		t.Fatal("root draft lost")
	}
	m = switchRoutingTest(t, m, "child")
	if m.textarea.Value() != "child draft" {
		t.Fatal("child draft lost")
	}
	if len(d.controls) != 0 || d.clients["root"].submitCount() != 0 || d.clients["child"].submitCount() != 0 {
		t.Fatal("navigation changed execution")
	}
}

func TestAgentPrintBarrierJoinsOldPrintAndRejectsStaleOutput(t *testing.T) {
	m, _ := routingTestModel()
	old := m.routing.generation
	m.routing.printing = true
	m.delivery.pending = &historyCommit{id: 1, generation: old, next: m.inline.clone()}
	cmd := m.openAgentView("child")
	next, _ := m.Update(cmd())
	m = modelValue(t, next)
	if m.sessionID != "root" || m.routing.pending == nil {
		t.Fatal("switch crossed outstanding print")
	}
	next, _ = m.Update(inlinePrintedMsg{generation: old, batch: 1})
	m = modelValue(t, next)
	if m.sessionID != "child" || !m.routing.switching {
		t.Fatal("switch not released by print acknowledgement")
	}
	next, _ = m.Update(inlinePrintMsg{generation: old, text: "OLD ROOT OUTPUT"})
	m = modelValue(t, next)
	if len(m.routing.printQueue) != 0 {
		t.Fatal("stale text entered child terminal")
	}
	var output bytes.Buffer
	reset := &resetAgentTerminal{}
	reset.SetStdout(&output)
	if err := reset.Run(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "\x1b[2J\x1b[H" {
		t.Fatal("reset did not clear only the managed frame")
	}
}

func TestAgentLateFailedReceiptReturnsDraftToOriginalView(t *testing.T) {
	m, _ := routingTestModel()
	m.registerOperation(protocol.NewSubmitInput("pending", protocol.SessionID(m.sessionID), "pending", "must not disappear", protocol.InputSteer), "message submitted")
	m = switchRoutingTest(t, m, "child")
	next, _ := m.Update(commandErrorMsg{commandID: "pending", sessionID: "root", err: errors.New("transport failed")})
	m = modelValue(t, next)
	if m.textarea.Value() != "" {
		t.Fatal("root failure polluted child composer")
	}
	m = switchRoutingTest(t, m, "root")
	if m.textarea.Value() != "must not disappear" || m.pendingSubmissionCount() != 0 {
		t.Fatal("failed original intent lost")
	}
}

func TestAgentSubmissionPinsViewedRun(t *testing.T) {
	m, d := routingTestModel()
	m = switchRoutingTest(t, m, "child")
	cmd := m.submitCommand(protocol.NewSubmitInput("command", "child", "input", "hello", protocol.InputFollowup), "send")
	_ = cmd()
	d.clients["child"].mu.Lock()
	sent := d.clients["child"].submits[0]
	d.clients["child"].mu.Unlock()
	if sent.SessionID != "child" || sent.ExpectedRunID != "run-child" || sent.Input.ID != "input" {
		t.Fatalf("identity lost: %+v", sent)
	}
}

func TestAgentCatalogRefreshDoesNotCreateMorePollingLoops(t *testing.T) {
	m, d := routingTestModel()
	for i := 0; i < 10; i++ {
		next, cmd := m.Update(agentCatalogMsg{directory: d, rows: d.rows})
		m = modelValue(t, next)
		if cmd != nil {
			t.Fatal("catalog result started an extra polling loop")
		}
	}
}

func TestAgentMidStreamSnapshotAndDeltaShareOneItem(t *testing.T) {
	m, _ := routingTestModel()
	item := protocol.TranscriptItem{ID: "stream:1:1", Kind: "assistant", Text: "prefix", Status: "streaming"}
	view := protocolSnapshot("root")
	view.Busy = true
	view.Transcript = []protocol.TranscriptItem{item}
	m.applySnapshot(view)
	item.Text = "prefix tail"
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventStream, Text: " tail", Transcript: &item})
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].text != "prefix tail" || !m.streaming {
		t.Fatalf("stream duplicated after attach: %+v", m.confirmedItems)
	}
	view.Transcript = []protocol.TranscriptItem{{ID: "committed", Kind: "assistant", Text: "prefix tail", Status: "completed"}}
	m.applySnapshot(view)
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].messageID != "committed" || m.streaming {
		t.Fatal("final snapshot did not replace live item")
	}
}

func TestAgentToolSnapshotAndEventDoNotDuplicate(t *testing.T) {
	m, _ := routingTestModel()
	item := protocol.TranscriptItem{ID: "tool:1:read", Kind: "tool", CallID: "read", Tool: "Read", Status: "running"}
	m.applyTranscript([]protocol.TranscriptItem{item})
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventToolStarted, Transcript: &item})
	item.Text, item.Status = "file contents", "success"
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventToolResult, Transcript: &item})
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].status != "success" {
		t.Fatalf("tool event duplicated: %+v", m.confirmedItems)
	}
}

func TestAgentCommittedStreamKeepsPrintAcknowledgement(t *testing.T) {
	m, _ := routingTestModel()
	live := historyCell{kind: "assistant", messageID: "stream:1:1", text: "already shown"}
	m.confirmedItems = []historyCell{live}
	m.inline.mark(live, 0)
	view := protocolSnapshot("root", protocol.MessageView{ID: "committed", Role: "assistant", Content: live.text})
	view.Transcript = []protocol.TranscriptItem{{ID: "committed", PreviousID: live.messageID, Kind: "assistant", Text: live.text, Status: "completed"}}
	m.applySnapshot(view)
	if !m.inline.seen(m.confirmedItems[0], 0) {
		t.Fatal("committed identity would print the same stream twice")
	}
}

// multiAgentModel builds a root with three children whose directory rows are
// deliberately shuffled, so spawn order must come from CreatedAt, not row order.
func multiAgentModel() (Model, *testSessionDirectory) {
	base := time.Now()
	ids := []protocol.SessionID{"a", "b", "c"}
	clients := map[protocol.SessionID]*recordingClient{}
	rows := []protocol.ChildSession{}
	root := &recordingClient{snapshot: protocolSnapshot("root")}
	clients["root"] = root
	for i, id := range ids {
		c := &recordingClient{snapshot: protocolSnapshot(string(id))}
		c.snapshot.RunID = protocol.RunID("run-" + string(id))
		clients[id] = c
		rows = append(rows, protocol.ChildSession{
			SessionID: id, ParentSessionID: "root", Title: "agent " + string(id),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Run:       protocol.RunView{ID: protocol.RunID("run-" + string(id)), Status: "running"},
		})
	}
	// Shuffle so the row order disagrees with spawn order.
	rows = []protocol.ChildSession{rows[2], rows[0], rows[1]}
	d := &testSessionDirectory{clients: clients, rows: rows}
	m := NewWithClient(root, "workspace", false)
	m.applySnapshot(root.snapshot)
	m.installSessionRouting(d)
	m.routing.rows = d.rows
	m.width, m.height = 100, 30
	m.layout()
	return m, d
}

func cycleRouting(t *testing.T, m Model, delta int) Model {
	t.Helper()
	cmd := m.cycleAgent(delta)
	if cmd == nil {
		t.Fatal("cycleAgent returned no command")
	}
	next, _ := m.Update(cmd())
	m = modelValue(t, next)
	if !m.routing.switching {
		t.Fatal("missing terminal barrier")
	}
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	return modelValue(t, next)
}

// ctrl+←/→ walks the sibling agents in spawn order and wraps at both ends, and
// the header carries the "N/M" ordinal for the active child.
func TestCycleAgentWalksSpawnOrder(t *testing.T) {
	m, _ := multiAgentModel()

	for _, want := range []protocol.SessionID{"a", "b", "c", "a"} {
		m = cycleRouting(t, m, 1)
		if m.sessionID != string(want) {
			t.Fatalf("forward cycle landed on %q, want %q", m.sessionID, want)
		}
	}
	m = cycleRouting(t, m, -1) // a -> c (wrap backwards)
	if m.sessionID != "c" {
		t.Fatalf("backward cycle landed on %q, want c", m.sessionID)
	}

	idx, total := m.agentOrdinal()
	if idx != 3 || total != 3 {
		t.Fatalf("ordinal = %d/%d, want 3/3", idx, total)
	}
	if !strings.Contains(m.headerPresentation(), "3/3") {
		t.Fatalf("header missing spawn ordinal:\n%s", m.headerPresentation())
	}
}

func TestChildContinuationAcceptsNewRunSnapshot(t *testing.T) {
	m, _ := routingTestModel()
	m = switchRoutingTest(t, m, "child")
	old := m.snapshot
	old.Revision.LogSeq = 100
	m.applySnapshot(old)
	next := old
	next.RunID = "continued-run"
	next.Revision.LogSeq = 90
	next.Busy = true
	next.Phase = protocol.PhasePreparing
	m.applySnapshot(next)
	if m.snapshot.RunID != next.RunID || !m.busy {
		t.Fatal("continuation snapshot was discarded as an older revision")
	}
	m.textarea.SetValue("followup")
	_, cmd := m.submit()
	if cmd == nil {
		t.Fatal("child input was not submitted")
	}
	_ = cmd()
	client := m.client.(*recordingClient)
	client.mu.Lock()
	defer client.mu.Unlock()
	sent := client.submits[len(client.submits)-1]
	if sent.Type != protocol.CommandSubmitInput || sent.SessionID != "child" || sent.ExpectedRunID != next.RunID || sent.Input.Text != "followup" {
		t.Fatalf("wrong child routing: %+v", sent)
	}
}
