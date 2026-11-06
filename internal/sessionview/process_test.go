package sessionview

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestProcessCompletionUpdatesOriginalCommandAcrossReplay(t *testing.T) {
	origin := ToolID("turn-1", "step-1", "bash")
	p := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "running"}, ProcessInfo: protocol.ProcessInfo{Origin: origin, Command: "build", Background: true}, Revision: 3}
	code := 1
	p.Status, p.ExitCode, p.Output = "exited", &code, "failed check"
	facts := []session.Event{
		session.ToolStarted{TurnID: "turn-1", StepID: "step-1", Call: session.ToolCall{CallID: "bash", ToolID: "Bash", Arguments: []byte(`{"command":"build","background":true}`)}},
		// Completion can be admitted before the original Bash return commits.
		session.ProcessFinished{State: p},
		session.ToolFinished{TurnID: "turn-1", StepID: "step-1", CallID: "bash", Status: "success", Process: &protocol.ProcessOutput{ID: p.ID, Status: "running"}, Result: session.ToolResult{CallID: "bash", Status: "success"}},
	}
	view := &Transcript{}
	view.Facts(facts)
	live, _ := view.Snapshot()
	if len(live) != 1 || live[0].Process == nil || !live[0].Process.Terminal() || *live[0].Process.ExitCode != 1 {
		t.Fatalf("lost actual process status: %+v", live)
	}
	var records []session.Record
	for i, fact := range facts {
		records = append(records, session.Record{Seq: session.Cursor(i + 1), Event: fact})
	}
	replayed, _ := FromRecords(records).Snapshot()
	if !reflect.DeepEqual(live, replayed) {
		t.Fatal("replay changed command state")
	}
	*live[0].Process.ExitCode = 99
	again, _ := view.Snapshot()
	if *again[0].Process.ExitCode != 1 {
		t.Fatal("snapshot mutated authoritative process state")
	}
}

func TestProcessReadIsLinkedToOriginalOccurrence(t *testing.T) {
	view := &Transcript{}
	for _, step := range []string{"step-1", "step-2"} {
		view.Facts([]session.Event{session.ToolStarted{TurnID: "turn-1", StepID: step, Call: session.ToolCall{CallID: "reused", ToolID: "Bash", Arguments: []byte(`{}`)}}})
	}
	p := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "running", Output: "fresh output"}, ProcessInfo: protocol.ProcessInfo{Origin: ToolID("turn-1", "step-1", "reused")}, Revision: 1}
	view.Facts([]session.Event{session.ProcessStarted{State: p}})
	view.Facts([]session.Event{session.ToolFinished{TurnID: "turn-1", StepID: "step-3", CallID: "read", Call: &session.ToolCall{CallID: "read", ToolID: "Process", Arguments: []byte(`{"action":"read"}`)}, Status: "success", Process: &p.ProcessOutput, Result: session.ToolResult{CallID: "read", Status: "success"}}})
	items, _ := view.Snapshot()
	if items[0].Process == nil || items[0].Process.Output != "fresh output" || items[1].Process != nil {
		t.Fatalf("linked output to the wrong occurrence: %+v", items)
	}
}

func TestProcessProgressCoalescesAndBoundsToolOutput(t *testing.T) {
	var q StreamQueue
	first := protocol.EventView{Kind: protocol.EventToolProgress, Tool: &protocol.ToolView{ID: "bash", Output: strings.Repeat("中", TextLimit)}}
	second := protocol.EventView{Kind: protocol.EventToolProgress, Tool: &protocol.ToolView{ID: "bash", Output: "last"}}
	q.Add(first, time.Now())
	q.Add(second, time.Now())
	items, lost := q.Drain(time.Now())
	if len(items) != 1 || !lost || len(items[0].Tool.Output) > TextLimit || !strings.HasSuffix(items[0].Tool.Output, "last") {
		t.Fatal("unbounded/lost tool output")
	}
	if second.Tool.Output != "last" {
		t.Fatal("coalescing mutated its input")
	}
}

func TestProcessWaitProgressUpdatesOriginalCommand(t *testing.T) {
	view := &Transcript{}
	origin := ToolID("turn-1", "step-1", "bash")
	state := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.runtime.owner.command", Status: "running"}, ProcessInfo: protocol.ProcessInfo{Origin: origin, Background: true}, Revision: 2}
	view.Facts([]session.Event{
		session.ToolStarted{TurnID: "turn-1", StepID: "step-1", Call: session.ToolCall{CallID: "bash", ToolID: "Bash", Arguments: []byte(`{}`)}},
		session.ProcessStarted{State: state},
		session.ToolFinished{TurnID: "turn-1", StepID: "step-1", CallID: "bash", Status: "success", Process: &state.ProcessOutput, Result: session.ToolResult{CallID: "bash", Status: "success", Text: "durable tool result"}},
		session.ToolStarted{TurnID: "turn-1", StepID: "step-2", ProcessID: state.ID, Call: session.ToolCall{CallID: "wait", ToolID: "Process", Arguments: []byte(`{}`)}},
	})
	event := protocol.EventView{Kind: protocol.EventToolProgress, TurnID: "turn-1", StepID: "step-2", Tool: &protocol.ToolView{ID: "wait", Output: "live output", Progress: &protocol.ProcessProgress{ID: state.ID, Offset: 0, Next: 11}}}
	view.Event(event)
	view.Event(event)
	item := view.EventItem(event)
	if item == nil || item.ID != origin || item.Text != "live output" || item.Process.Output != "live output" {
		t.Fatalf("wait progress did not reach original command: %+v", item)
	}
	state.Status = "exited"
	state.Output = "final output"
	state.Revision++
	view.Facts([]session.Event{session.ProcessFinished{State: state}})
	item = view.EventItem(event)
	if item.Process.Output != "final output" || item.Text != "durable tool result" {
		t.Fatalf("transient progress replaced durable audit result: %+v", item)
	}
	view.Event(event)
	if item = view.EventItem(event); !item.Process.Terminal() || item.Process.Output != "final output" {
		t.Fatal("late progress regressed terminal state")
	}
}
