package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

// Opt-in measurement against a real checkout. Only reads workspace files;
// all snapshots and session records are written into the test directory.
func TestProfileFinalization(t *testing.T) {
	root := os.Getenv("CCDP_PROFILE_WORKSPACE")
	if root == "" {
		t.Skip("set CCDP_PROFILE_WORKSPACE to measure a real workspace")
	}
	cfg := config.Default()
	cfg.Workspace, cfg.SessionDir = root, t.TempDir()
	cfg.PermissionMode = "bypass"
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(true)
	cfg.MCPServers = nil
	a, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	measure := func(name string, work func() error) {
		start := time.Now()
		if err := work(); err != nil {
			t.Fatal(name, err)
		}
		t.Logf("%s: %s", name, time.Since(start))
	}
	measure("inventory", func() error {
		paths, err := workspace.Inventory(context.Background(), root, a.sandbox.Snapshot())
		t.Logf("inventory files: %d", len(paths))
		return err
	})
	var point session.RecoveryPointRecorded
	measure("checkpoint_before", func() error {
		var err error
		point, err = a.captureRecoveryPoint(context.Background(), "profile-turn", "profile")
		return err
	})
	measure("checkpoint_after_unchanged", func() error { return a.sealRecoveryPoint(point) })
	measure("turn_finished_fact", func() error { return a.persistTurnFinished("profile-turn", "success", "", time.Second) })
	a.turnUserMsg, a.turnSummary = "profile", "profile completed"
	measure("memory", a.recordMemory)
	measure("session_save", a.Save)
}
