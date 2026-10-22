package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func TestMemorySessionReopenPreservesCheckpointAndPreparedRestore(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		name := "prepare-after-reopen"
		if prepared {
			name = "apply-prepared-before-reopen"
		}
		t.Run(name, func(t *testing.T) {
			cfg := isolatedTestConfig(t, "memory-coding")
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			path := filepath.Join(cfg.Workspace, "sample.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			history := []messages.Message{{ID: "before", Role: "user", Content: "before checkpoint"}}
			if err := a.persistHistory(history, "initial-history"); err != nil {
				t.Fatal(err)
			}
			point, err := a.captureRecoveryPoint(context.Background(), "turn", "checkpoint")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("after\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.persistHistory(append(history, messages.Message{ID: "after", Role: "user", Content: "later message"}), "later-history"); err != nil {
				t.Fatal(err)
			}
			workflow := protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "both", Scope: "snapshot"}
			var planID string
			if prepared {
				out, err := runCoding(t, a, workflow)
				if err != nil {
					t.Fatal(err)
				}
				planID = deliveryID(t, out)
			}
			// Switch away and close the old handle before returning. The catalog
			// must own the data, independently of the old Agent and its locks.
			id := a.SessionID()
			forkID, err := a.Fork(-1)
			if err != nil {
				t.Fatal(err)
			}
			fork, err := a.OpenSession(forkID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(fork.Close)
			if err := a.CloseContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := fork.OpenSession(id)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.Close)
			if err := fork.CloseContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			points, err := reopened.recoveryPoints()
			if err != nil || len(points) != 1 || points[0].ID != point.ID {
				t.Fatalf("checkpoint records: %+v, %v", points, err)
			}
			if !prepared {
				out, err := runCoding(t, reopened, workflow)
				if err != nil {
					t.Fatal(err)
				}
				planID = deliveryID(t, out)
			}
			if _, err := runCoding(t, reopened, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: planID}); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "before\n" {
				t.Fatalf("restored content=%q, %v", content, err)
			}
			if got := reopened.History(); len(got) != 1 || got[0].ID != "before" {
				t.Fatalf("restored history=%+v", got)
			}
		})
	}
}

func TestMemorySessionReopenPreservesPreparedDelivery(t *testing.T) {
	cfg := isolatedTestConfig(t, "memory-delivery")
	root := cfg.Workspace
	git(t, root, "init", "-q")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	git(t, root, "config", "commit.gpgsign", "false")
	path := filepath.Join(root, "chosen.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-qm", "base")
	if err := os.WriteFile(path, []byte("selected change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	out, err := runCoding(t, a, protocol.WorkflowCommand{
		Kind: protocol.WorkflowCommitPushPR, Action: "prepare",
		Message: "selected", Mode: "commit", Paths: []string{"chosen.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	planID := deliveryID(t, out)
	reopened, err := a.OpenSession(a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out, err := runCoding(t, reopened, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: planID}); err != nil {
		t.Fatalf("apply: %v (%s)", err, out)
	}
	if got := git(t, root, "show", "HEAD:chosen.txt"); got != "selected change\n" {
		t.Fatalf("committed content=%q", got)
	}
}
