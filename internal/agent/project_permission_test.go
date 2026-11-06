package agent

import (
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

func TestPermissionChangePersistsProjectAndChildInherits(t *testing.T) {
	a := newRuntimeAgent(t)
	cmd := protocol.Command{ID: "project-mode", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetPermissionPolicy, PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: "manual"}}}
	if receipt := a.applyCommand(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	fresh := config.Default()
	fresh.Workspace = a.services.cfg.Workspace
	fresh.SessionDir = a.services.cfg.SessionDir
	effective, err := buildEffectiveConfig(fresh, nil, false, Options{})
	if err != nil || effective.PermissionMode != "manual" {
		t.Fatalf("project reload: %s %v", effective.PermissionMode, err)
	}
	cmd.ID = "project-auto"
	cmd.PermissionPolicy.Policy.Mode = "auto"
	if receipt := a.applyCommand(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	effective, err = buildEffectiveConfig(fresh, nil, false, Options{})
	if err != nil || effective.PermissionMode != "auto" {
		t.Fatalf("auto reload: %s %v", effective.PermissionMode, err)
	}
	cfg, opts, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "auto" {
		t.Fatal(cfg.PermissionMode)
	}
	decision, _ := opts.Permissions.Check("Write", map[string]any{"file_path": a.services.cfg.Workspace + "/test.txt"}, permissions.KindWrite)
	if decision != permissions.DecisionAllow {
		t.Fatalf("child write asks for approval: %s", decision)
	}
}
