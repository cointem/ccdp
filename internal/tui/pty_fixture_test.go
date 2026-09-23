package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

// Run by testdata/pty_smoke.py in an OS PTY, never during the normal test suite.
func TestPTYRearchitectureFixture(t *testing.T) {
	if os.Getenv("CCDP_TUI_PTY_FIXTURE") != "1" {
		t.Skip("PTY driver only")
	}
	snapshot := protocolSnapshot("pty-root")
	snapshot.Settings.ContextWindow = 200000
	snapshot.Transcript = []protocol.TranscriptItem{
		{ID: "u", Kind: "user", Text: "Inspect the files"},
		{ID: "a", Kind: "assistant", Status: "completed", Text: "I will inspect the project. 中文保持可读。"},
		{ID: "r1", Kind: "tool", Tool: "Read", CallID: "read-one", Status: "success", Args: json.RawMessage(`{"file_path":"/workspace/ccdp/main.go"}`), Text: "package main"},
		{ID: "r2", Kind: "tool", Tool: "Read", CallID: "read-two", Status: "success", Args: json.RawMessage(`{"file_path":"/workspace/ccdp/go.mod"}`), Text: "module ccdp"},
		{ID: "b", Kind: "tool", Tool: "Bash", CallID: "exec", Status: "success", Args: json.RawMessage(`{"command":"go test ./..."}`), Text: "first\nsecond\nthird\nfourth\nfifth\nsixth\nlast"},
		{ID: "done", Kind: "assistant", Status: "completed", Text: "PTY_RESULT_COMPLETE\n[file.go](src/file.go)"},
	}
	snapshot.Transcript = append(snapshot.Transcript, protocol.TranscriptItem{ID: "task", Kind: "tool", Tool: "Task", CallID: "task-call", Status: "running"})
	client := &recordingClient{snapshot: snapshot}
	m := NewWithClient(client, "/workspace/ccdp", false)
	childOne, childTwo := protocolSnapshot("child-one"), protocolSnapshot("child-two")
	childOne.Transcript = []protocol.TranscriptItem{{ID: "one-answer", Kind: "assistant", Text: "CHILD_ONE_VIEW", Status: "completed"}}
	childTwo.Transcript = []protocol.TranscriptItem{{ID: "two-answer", Kind: "assistant", Text: "CHILD_TWO_VIEW", Status: "completed"}}
	m.installSessionRouting(&testSessionDirectory{clients: map[protocol.SessionID]*recordingClient{
		"pty-root": client, "child-one": {snapshot: childOne}, "child-two": {snapshot: childTwo},
	}, rows: []protocol.ChildSession{
		{SessionID: "child-one", ParentSessionID: "pty-root", ParentCallID: "task-call", Title: "FIRST_AGENT", Run: protocol.RunView{Status: "running"}},
		{SessionID: "child-two", ParentSessionID: "pty-root", ParentCallID: "task-call", Title: "SECOND_AGENT", Run: protocol.RunView{Status: "running"}},
	}})
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	go func() {
		time.Sleep(500 * time.Millisecond)
		live := snapshot
		live.Transcript = append(append([]protocol.TranscriptItem(nil), snapshot.Transcript...),
			protocol.TranscriptItem{ID: "dynamic-user", Kind: "user", Text: "LIVE_USER_MESSAGE"},
			protocol.TranscriptItem{ID: "dynamic-answer", Kind: "assistant", Status: "running", Text: "LIVE_STREAM"})
		live.Revision.LogSeq += 1
		client.publish(protocol.Update{Type: protocol.UpdateState, Snapshot: &live})
		time.Sleep(500 * time.Millisecond)
		complete := live
		complete.Transcript = append([]protocol.TranscriptItem(nil), live.Transcript...)
		complete.Transcript[len(complete.Transcript)-1].Text = "LIVE_RESULT_COMPLETE"
		complete.Transcript[len(complete.Transcript)-1].Status = "completed"
		complete.Transcript = append(complete.Transcript, protocol.TranscriptItem{ID: "thought-detail", Kind: "thinking", Status: "completed", Text: strings.Repeat("DETAIL_SENTINEL\n", 80)})
		complete.Revision.LogSeq += 1
		client.mu.Lock()
		client.snapshot = complete
		client.mu.Unlock()
		client.publish(protocol.Update{Type: protocol.UpdateState, Snapshot: &complete})
	}()
	final, err := NewProgram(m).Run()
	if err != nil {
		t.Fatal(err)
	}
	var current Model
	switch value := final.(type) {
	case Model:
		current = value
	case *Model:
		current = *value
	default:
		t.Fatalf("unexpected model %T", final)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if current.delivery.pending != nil {
		t.Fatal("unacknowledged history at exit")
	}
}
