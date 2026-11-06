package replay

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessFactsRoundTripAndCompactionPreservesDelivery(t *testing.T) {
	state := New()
	p := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.runtime.owner.command", Status: "registered"}, ProcessInfo: protocol.ProcessInfo{Origin: "tool:turn:step:call", Command: "build"}, Revision: 1}
	registered := session.ProcessRegistered{State: p}
	p.Status = "running"
	p.Revision++
	started := session.ProcessStarted{State: p}
	p.Status = "exited"
	p.ResultID = "result." + p.ID
	p.OutputID = strings.Repeat("a", 64)
	p.Output = "ONE_RESULT"
	p.Revision++
	finished := session.ProcessFinished{State: p, Result: session.BlobRef{Hash: p.OutputID, Size: 100, MediaType: "application/json"}}
	for _, e := range []session.Event{registered, started, finished} {
		frozen, err := session.CloneEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(frozen); err != nil {
			t.Fatal(err)
		}
	}
	clone := state.Clone()
	pending := clone.PendingProcessResults()
	if len(pending) != 1 || pending[0].ID != p.ID {
		t.Fatalf("pending completion was not replayed/cloned: %+v", pending)
	}
	pending[0].Output = "mutated"
	if state.PendingProcessResults()[0].Output != p.Output {
		t.Fatal("pending delivery shares mutable output")
	}
	clone.Processes[p.ID].Result.Hash = "mutated"
	if state.Processes[p.ID].Result.Hash != p.OutputID {
		t.Fatal("clone shares result references")
	}
	raw, _ := json.Marshal(p.ProcessOutput)
	if err := state.Apply(session.ToolFinished{TurnID: "turn", StepID: "step", CallID: "call", Status: "success", CompletionIDs: []string{p.ID}, Process: &p.ProcessOutput, Result: session.ToolResult{CallID: "call", Status: "success", Text: string(raw)}}); err != nil {
		t.Fatal(err)
	}
	recovered := state.Recovered()
	if len(recovered.History) != 1 || strings.Contains(recovered.History[0].Content, p.Output) || !strings.Contains(recovered.History[0].Content, p.OutputID) {
		t.Fatal("crash recovery duplicated pending completion", recovered.History)
	}
	if err := state.Apply(session.ProcessResultsProjected{IDs: []string{p.ID}, Outputs: []protocol.ProcessOutput{p.ProcessOutput}}); err != nil {
		t.Fatal(err)
	}
	if len(state.PendingProcessResults()) != 0 || len(clone.PendingProcessResults()) != 1 {
		t.Fatal("projected queue was not pruned independently of its clone")
	}
	if state.History[0].InternalSource != "process_completion" {
		t.Fatal("process output was misclassified as user input")
	}
	summary, err := session.EncodeMessage(messages.Message{ID: "summary", Role: messages.RoleUser, Content: "compacted", SourceOutputs: []messages.OutputReference{*state.History[0].OutputRef}})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(session.ContextCompacted{Summary: summary}); err != nil {
		t.Fatal(err)
	}
	if !state.Processes[p.ID].Projected || state.Processes[p.ID].Result.Hash != p.OutputID || len(state.History[0].SourceOutputs) != 1 {
		t.Fatal("compaction lost process delivery or archive index")
	}
	if err := state.Apply(session.ProcessResultsProjected{IDs: []string{p.ID}, Outputs: []protocol.ProcessOutput{p.ProcessOutput}}); err == nil {
		t.Fatal("replay accepted duplicate delivery")
	}
}
