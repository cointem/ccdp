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
	if cfg.PermissionMode != "auto" {
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
	if cfg.PermissionMode != "manual" {
		t.Fatal("subdirectory did not inherit project preference")
	}
	cfg.Workspace = other
	cfg.PermissionMode = "manual"
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "manual" {
		t.Fatal("permission leaked to other project")
	}
	cfg.Workspace = repo
	cfg.markSource("permission_mode", SourceCLI, "cli", false)
	if err := cfg.ApplyProjectPermission(); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "manual" {
		t.Fatal("project overrode CLI")
	}
}

func TestPermissionNamesAreCanonical(t *testing.T) {
	for _, name := range []string{"auto", "manual"} {
		cfg := Default()
		cfg.PermissionMode = name
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.PermissionMode != name {
			t.Fatalf("%s changed to %s", name, cfg.PermissionMode)
		}
	}
	for _, name := range []string{"edits", "bypass", "plan", "default", "acceptEdits", "bypassPermissions"} {
		cfg := Default()
		cfg.PermissionMode = name
		if err := cfg.Validate(); err == nil {
			t.Fatalf("retired permission name %q was accepted", name)
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
	if cfg.PermissionMode != "manual" {
		t.Fatalf("host permission persistence broken: %s", cfg.PermissionMode)
	}
}
