package protocol

import "testing"

func TestStateExplicitReplacementPreservesSnapshotOnlyData(t *testing.T) {
	view := SessionView{SessionID: "root", History: []MessageView{{ID: "old", Content: "original"}}, Catalog: CatalogSnapshot{Version: 9}, Goal: &Goal{ID: "old"}, Approval: &ApprovalView{ID: "old"}, Question: &QuestionRequest{}, PendingInputs: []InputView{{ID: "old"}}}
	view.ApplyState(SessionState{SessionID: "root", Busy: true, HistoryLength: 200, OutstandingWork: 2, Usage: UsageSnapshot{TurnCount: 4}})
	if view.Goal != nil || view.Approval != nil || view.Question != nil || len(view.PendingInputs) != 0 {
		t.Fatal("nil state fields must clear pending decisions")
	}
	if len(view.History) != 1 || view.Catalog.Version != 9 {
		t.Fatal("state discarded snapshot-only data")
	}
	if view.HistoryLength != 200 || view.OutstandingWork != 2 || !view.Busy || view.Usage.TurnCount != 4 {
		t.Fatalf("incomplete state projection: %+v", view)
	}
}
