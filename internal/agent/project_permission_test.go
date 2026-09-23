package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"testing"
)

func TestPermissionChangePersistsProjectAndChildInherits(t *testing.T) {
	a := newRuntimeAgent(t)
	cmd := protocol.Command{ID: "project-mode", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetPermissionPolicy, PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: "manual"}}}
	if receipt := a.applyCommand(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	fresh := config.Default()
	fresh.Workspace = a.cfg.Workspace
	fresh.SessionDir = a.cfg.SessionDir
	effective, err := buildEffectiveConfig(fresh, nil, false, Options{})
	if err != nil || effective.PermissionMode != "default" {
		t.Fatalf("project reload: %s %v", effective.PermissionMode, err)
	}
	cmd.ID = "project-edits"
	cmd.PermissionPolicy.Policy.Mode = "edits"
	if receipt := a.applyCommand(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	effective, err = buildEffectiveConfig(fresh, nil, false, Options{})
	if err != nil || effective.PermissionMode != "acceptEdits" {
		t.Fatalf("edits reload: %s %v", effective.PermissionMode, err)
	}
	cfg, opts, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "acceptEdits" {
		t.Fatal(cfg.PermissionMode)
	}
	decision, _ := opts.Permissions.Check("Write", map[string]any{"file_path": a.cfg.Workspace + "/test.txt"})
	if decision != permissions.DecisionAllow {
		t.Fatalf("child write asks for approval: %s", decision)
	}
}
