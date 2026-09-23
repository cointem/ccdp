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
