package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectPermissionsRememberAndIsolate(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sub := filepath.Join(repo, "src")
	other := filepath.Join(base, "other")
	for _, p := range []string{filepath.Join(repo, ".git"), sub, other} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Default()
	cfg.SessionDir = filepath.Join(base, "sessions")
	cfg.Workspace = repo
	if cfg.PermissionMode != "acceptEdits" {
		t.Fatal(cfg.PermissionMode)
	}
	if err := cfg.SaveProjectPermission("manual"); err != nil {
		t.Fatal(err)
	}
	canonical, err := NormalizeProjectRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(string(filepath.Separator), canonical)
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(cfg.ProjectPermissionsDir(), relative, "permission.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("readable project permission record missing: %v", err)
	}
	cfg.Workspace = sub
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "default" {
		t.Fatal("subdirectory did not inherit project preference")
	}
	cfg.Workspace = other
	cfg.PermissionMode = "acceptEdits"
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "acceptEdits" {
		t.Fatal("permission leaked to other project")
	}
	cfg.Workspace = repo
	cfg.markSource("permission_mode", SourceCLI, "cli", false)
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "acceptEdits" {
		t.Fatal("project overrode CLI")
	}
}

func TestPermissionNamesRemainCompatible(t *testing.T) {
	for name, want := range map[string]string{"manual": "default", "default": "default", "edits": "acceptEdits", "acceptEdits": "acceptEdits", "bypass": "bypassPermissions", "bypassPermissions": "bypassPermissions"} {
		cfg := Default()
		cfg.PermissionMode = name
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.PermissionMode != want {
			t.Fatalf("%s normalized to %s", name, cfg.PermissionMode)
		}
	}
}

func TestSandboxProtectsProjectPermissionRecords(t *testing.T) {
	cfg := Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "state", "sessions")
	record, _, err := cfg.projectPermissionPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		if existing {
			if err := cfg.SaveProjectPermission("manual"); err != nil {
				t.Fatal(err)
			}
		}
		policy := cfg.Sandbox()
		if _, err := policy.ResolveWrite(record); err == nil {
			t.Fatalf("permission record writable (existing=%v): %s", existing, record)
		}
		if _, err := policy.ResolveWriteEntry(record); err == nil {
			t.Fatalf("permission record replaceable (existing=%v)", existing)
		}
		if _, err := policy.ResolveWrite(filepath.Join(cfg.Workspace, "ordinary.txt")); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "default" {
		t.Fatalf("host permission persistence broken: %s", cfg.PermissionMode)
	}
}
