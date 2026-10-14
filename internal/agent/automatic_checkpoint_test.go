package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

func addUnrelatedLargeFile(t *testing.T, root string) {
	t.Helper()
	f, err := os.Create(filepath.Join(root, "unrelated-large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(513 << 20); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultReadOnlyTurnDoesNotCaptureWorkspace(t *testing.T) {
	a, events := newTestAgent(t, &fakeLLM{script: []string{"text:done"}})
	addUnrelatedLargeFile(t, a.services.cfg.Workspace)
	go a.Run()
	submitTestInput(t, a, "answer without editing")
	drainUntil(t, events, 5*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
	view, err := a.Snapshot(context.Background())
	if err != nil || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("read-only turn failed: %+v, %v", view.LastTurn, err)
	}
	points, err := a.recoveryPoints()
	if err != nil || len(points) != 0 {
		t.Fatalf("read-only turn created recovery data: %+v, %v", points, err)
	}
}

func TestDefaultWriteAndUndoOnlyCaptureChangedFiles(t *testing.T) {
	a, events := newTestAgent(t, &fakeLLM{script: []string{"tool:Read|file_path=chosen.txt", "tool:Write|file_path=chosen.txt;mode=replace;content=agent\ntwo\nthree\n", "text:done"}})
	root := a.services.cfg.Workspace
	path := filepath.Join(root, "chosen.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	addUnrelatedLargeFile(t, root)
	go a.Run()
	submitTestInput(t, a, "change first line")
	drainUntil(t, events, 5*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
	points, err := a.recoveryPoints()
	if err != nil || len(points) != 1 || points[0].Before != "" || points[0].After != "" {
		t.Fatalf("expected one file-only recovery point: %+v, %v", points, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "agent\ntwo\nthree\n" {
		t.Fatalf("write failed: %q, %v", data, err)
	}
	if err := os.WriteFile(path, []byte("agent\ntwo\nuser\n"), 0644); err != nil {
		t.Fatal(err)
	}
	apply := func(point string) {
		t.Helper()
		out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point, Mode: "code"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: deliveryID(t, out)}); err != nil {
			t.Fatal(err)
		}
	}
	apply(points[0].ID)
	if data, err := os.ReadFile(path); err != nil || string(data) != "one\ntwo\nuser\n" {
		t.Fatalf("undo lost user edits: %q, %v", data, err)
	}
	points, err = a.recoveryPoints()
	if err != nil || len(points) != 2 || points[0].Before != "" {
		t.Fatalf("restore backup should also be file-only: %+v, %v", points, err)
	}
	apply(points[0].ID)
	if data, err := os.ReadFile(path); err != nil || string(data) != "agent\ntwo\nuser\n" {
		t.Fatalf("undo of restore failed: %q, %v", data, err)
	}
}

func TestFullAutomaticCheckpointRequiresOptIn(t *testing.T) {
	a, root := codingFixture(t)
	addUnrelatedLargeFile(t, root)
	a.services.cfg.AutomaticCheckpoints = config.BoolPtr(true)
	_, err := a.beginAutomaticCheckpoint(context.Background(), "turn-1", "full snapshot", "")
	if err == nil || !strings.Contains(err.Error(), "snapshot limit") {
		t.Fatalf("explicit full capture did not inspect workspace: %v", err)
	}
}

func TestConversationRestoreBackupDoesNotPretendToCaptureFiles(t *testing.T) {
	a, root := codingFixture(t)
	point, err := a.captureRecoveryPoint(context.Background(), "turn-1", "before conversation")
	if err != nil {
		t.Fatal(err)
	}
	addUnrelatedLargeFile(t, root)
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: deliveryID(t, out)}); err != nil {
		t.Fatal(err)
	}
	points, err := a.recoveryPoints()
	if err != nil || len(points) != 2 || points[0].Before != "" {
		t.Fatalf("conversation backup advertises a full file snapshot: %+v, %v", points, err)
	}
	if _, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: points[0].ID, Mode: "code", Scope: "snapshot"}); err == nil {
		t.Fatal("conversation-only backup accepted full workspace restore")
	}
}
