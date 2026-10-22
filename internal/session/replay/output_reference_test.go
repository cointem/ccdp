package replay

import (
	"strings"
	"testing"

	"ccdp/internal/session"
)

func TestEmptyBudgetProjectionSurvivesReplay(t *testing.T) {
	state := New()
	if err := state.Apply(session.ToolResultsProjected{TurnID: "turn", StepID: "step", Results: []session.ProjectedToolResult{
		{CallID: "failed", Status: "error", Text: "", DisplayTruncated: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 1 || state.History[0].Content != "" || state.History[0].ToolStatus != "error" || !state.History[0].DisplayTruncated {
		t.Fatalf("replay invented output or lost status: %+v", state.History)
	}
	state.FinalizeRecovery()
	if state.History[0].Content != "" {
		t.Fatal("recovery expanded empty projection")
	}
}

func TestProjectedAndRecoveredToolOutputsKeepOriginalReference(t *testing.T) {
	for _, projected := range []bool{false, true} {
		state := New()
		finished := session.ToolFinished{TurnID: "turn", StepID: "step", CallID: "call", Status: "success",
			RawOutput:        &session.BlobRef{Hash: strings.Repeat("a", 64), Size: 20000, MediaType: "text/plain"},
			Result:           session.ToolResult{CallID: "call", Status: "success", Text: "Error: legitimate output"},
			DisplayTruncated: true, DataError: "invalid structured payload"}
		if err := state.Apply(finished); err != nil {
			t.Fatal(err)
		}
		if projected {
			if err := state.Apply(session.ToolResultsProjected{TurnID: "turn", StepID: "step", Results: []session.ProjectedToolResult{
				{CallID: "call", Status: "success", Text: "Error: legitimate output", DisplayTruncated: true, DataError: finished.DataError},
			}}); err != nil {
				t.Fatal(err)
			}
		} else {
			state.FinalizeRecovery()
		}
		if len(state.History) != 1 {
			t.Fatalf("missing recovered result: %+v", state.History)
		}
		got := state.History[0]
		if got.OutputRef == nil || got.OutputRef.Hash != finished.RawOutput.Hash || got.ToolStatus != "success" ||
			got.Content != finished.Result.Text || !got.DisplayTruncated || got.DataError != finished.DataError {
			t.Fatalf("lost result metadata: %+v", got)
		}
		finished.RawOutput.Hash = "changed"
		if state.History[0].OutputRef.Hash == "changed" {
			t.Fatal("replay retained mutable raw reference")
		}
	}
}
