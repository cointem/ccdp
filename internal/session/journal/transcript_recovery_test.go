package journal

import (
	"context"
	"testing"

	"ccdp/internal/session"
	"ccdp/internal/sessionview"
)

func TestTranscriptRecoveryBoundarySurvivesLiveCommits(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk", true: "memory"}[memory], func(t *testing.T) {
			options := OpenOptions{SessionID: "recovered-tools", SessionDir: t.TempDir(), NoSessionPersistence: memory}
			first, err := Open(options)
			if err != nil {
				t.Fatal(err)
			}
			start := func(id string) session.ToolStarted {
				return session.ToolStarted{TurnID: "turn-" + id, StepID: "step-" + id, Fingerprint: id,
					Call: session.ToolCall{CallID: id, ToolID: "Read", Arguments: []byte(`{"file_path":"a.go"}`)}}
			}
			if _, err := first.CommitEvents("old", start("old")); err != nil {
				t.Fatal(err)
			}
			oldRecords, artifacts, err := first.ReadSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			first.Close()
			reopened, err := Open(options)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if memory {
				if err := reopened.RestoreMemoryRun(oldRecords, artifacts); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reopened.CommitEvents("live", start("live")); err != nil {
				t.Fatal(err)
			}
			records, _, recoveredThrough, err := reopened.ReadSnapshotWithRecovery()
			if err != nil {
				t.Fatal(err)
			}
			if recoveredThrough != oldRecords[len(oldRecords)-1].Seq || recoveredThrough >= records[len(records)-1].Seq {
				t.Fatalf("recovery boundary moved with new commits: %d", recoveredThrough)
			}
			live, _ := reopened.TranscriptView().Snapshot()
			if len(live) != 2 || live[0].Status != "unknown" || live[1].Status != "running" {
				t.Fatalf("live transcript=%+v", live)
			}
			// The old tool is outside the latest page. Its status must agree
			// with the writer when a reader reconstructs that older page.
			page := sessionview.NewTranscript(2, 1)
			if err := page.ReplayRecords(context.Background(), records, recoveredThrough); err != nil {
				t.Fatal(err)
			}
			items, _ := page.Snapshot()
			if len(items) != 1 || items[0].CallID != "old" || items[0].Status != "unknown" {
				t.Fatalf("older page=%+v", items)
			}
		})
	}
}
