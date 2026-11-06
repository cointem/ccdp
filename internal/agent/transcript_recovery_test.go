package agent

import (
	"context"
	"encoding/json"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/session/journal"
)

func TestTranscriptReadersShareLiveAndArchivedRecoveryBoundary(t *testing.T) {
	ctx := context.Background()
	cfg := isolatedTestConfig(t, "transcript-recovery")
	cfg.SessionID = "recovered-child"
	writer, err := journal.Open(journal.OpenOptions{SessionID: cfg.SessionID, NoSessionPersistence: true})
	if err != nil {
		t.Fatal(err)
	}
	start := func(id string) session.ToolStarted {
		return session.ToolStarted{TurnID: "turn-" + id, StepID: "step-" + id, Fingerprint: id,
			Call: session.ToolCall{CallID: id, ToolID: "Read", Arguments: []byte(`{"file_path":"a.go"}`)}}
	}
	if _, err := writer.CommitEvents("old-tool", start("old")); err != nil {
		t.Fatal(err)
	}
	records, blobs, err := writer.ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	initial, err := archive.FromRecords(records, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := newAgentWithOptions(&cfg, initial, Options{EffectiveConfigFrozen: true, memoryResume: records, memoryBlobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if _, err := child.persistenceHandle().CommitEvents("live-tool", start("live")); err != nil {
		t.Fatal(err)
	}
	rootCfg := isolatedTestConfig(t, "transcript-root")
	root, err := testNewWithOptions(t, &rootCfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s := root.services.supervisor
	row := protocol.ChildSession{SessionID: protocol.SessionID(cfg.SessionID), ParentSessionID: protocol.SessionID(root.SessionID()),
		Run: protocol.RunView{ID: "recovered-run", Status: "succeeded"}}
	done := make(chan struct{})
	close(done)
	run := &managedRun{agent: child, done: done, fact: session.ChildRunRecorded{Child: row}}
	s.directory.Restore(row, run, true)
	check := func(wantLive string) {
		t.Helper()
		page, err := s.ReadTranscript(ctx, row.SessionID, 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := s.readAgentTranscript(ctx, protocol.AgentReadPage{AgentID: row.SessionID},
			agentReadCursor{Agent: row.SessionID, View: "transcript"}, 16<<10)
		if err != nil {
			t.Fatal(err)
		}
		var agentPage protocol.AgentReadPage
		if err := json.Unmarshal([]byte(encoded), &agentPage); err != nil {
			t.Fatal(err)
		}
		for _, items := range [][]protocol.TranscriptItem{page.Items, agentPage.Items} {
			if len(items) != 2 || items[0].Status != "unknown" || items[1].Status != wantLive {
				t.Fatalf("transcript reader disagrees with recovery boundary: %+v", items)
			}
		}
	}
	check("running")
	records, blobs, err = child.persistenceHandle().ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// The same immutable log becomes wholly archived after its runtime goes
	// away; both read APIs must now stop claiming that its last tool is live.
	run.mu.Lock()
	run.agent, run.memory, run.memoryBlobs = nil, records, blobs
	run.mu.Unlock()
	check("unknown")
}
