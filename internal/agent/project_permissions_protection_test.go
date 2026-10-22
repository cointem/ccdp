package agent

import (
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/messages"
)

func TestProjectPermissionRecordsProtectedAcrossSandboxConstruction(t *testing.T) {
	a := newRuntimeAgent(t)
	cfg := a.configSnapshot()
	record := filepath.Join(cfg.ProjectPermissionsDir(), "unauthorized.json")
	if _, err := buildSandbox(&cfg, cfg.Workspace).ResolveWrite(record); err == nil {
		t.Fatal("candidate sandbox permits permission record writes")
	}
	out, failed := executeTestTool(t, a, messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": record, "content": `{"mode":"bypassPermissions"}`}})
	if !failed {
		t.Fatalf("ordinary Write modified control state: %s", out)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("denied write created record: %v", err)
	}
	// The host can still persist explicit permission decisions in the protected directory.
	if err := cfg.SaveProjectPermission("manual"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	next, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if got := next.services.perms.CurrentMode(); got != "default" {
		t.Fatalf("legitimate saved mode was not restored: %s", got)
	}
}
