package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
)

func TestDevelopmentConsentOnceAndExplicitModes(t *testing.T) {
	newConfig := func() config.Config {
		c := config.Default()
		c.Workspace = t.TempDir()
		c.SessionDir = filepath.Join(t.TempDir(), "sessions")
		return c
	}
	cfg := newConfig()
	var output bytes.Buffer
	if err := prepareDevelopmentEnvironment(&cfg, strings.NewReader(""), &output, false); err == nil {
		t.Fatal("headless silently accepted new boundary")
	}
	if err := prepareDevelopmentEnvironment(&cfg, strings.NewReader("yes\n"), &output, true); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := prepareDevelopmentEnvironment(&cfg, strings.NewReader(""), &output, false); err != nil || output.Len() != 0 {
		t.Fatalf("accepted boundary prompted again: %v %q", err, output.String())
	}
	for _, mode := range []permissions.Mode{permissions.ModeManual, permissions.ModeManual, permissions.ModeAuto} {
		c := newConfig()
		c.PermissionMode = string(mode)
		if err := prepareDevelopmentEnvironment(&c, strings.NewReader("yes\n"), &output, true); err != nil || c.PermissionMode != string(mode) || !c.LocalDevelopmentNetwork {
			t.Fatalf("explicit %s changed: %+v %v", mode, c.PermissionMode, err)
		}
		if err := prepareDevelopmentEnvironment(&c, strings.NewReader(""), &output, false); err != nil {
			t.Fatalf("accepted %s prompted again: %v", mode, err)
		}
	}
	declined := newConfig()
	if err := prepareDevelopmentEnvironment(&declined, strings.NewReader("no\n"), &output, true); err != nil || declined.PermissionMode != string(permissions.ModeManual) || declined.LocalDevelopmentNetwork {
		t.Fatalf("decline: %s %v", declined.PermissionMode, err)
	}
	explicit := newConfig()
	if err := explicit.ApplyCLIOverride("permission_mode", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDevelopmentEnvironment(&explicit, strings.NewReader(""), &output, false); err != nil {
		t.Fatal(err)
	}
}
