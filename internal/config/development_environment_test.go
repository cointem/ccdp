package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevelopmentConsentFollowsEffectiveBoundary(t *testing.T) {
	cfg := Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
	if yes, err := cfg.DevelopmentEnvironmentAccepted(); err != nil || yes {
		t.Fatalf("fresh boundary: %v %v", yes, err)
	}
	if err := cfg.AcceptDevelopmentEnvironment(); err != nil {
		t.Fatal(err)
	}
	if yes, err := cfg.DevelopmentEnvironmentAccepted(); err != nil || !yes {
		t.Fatalf("saved boundary: %v %v", yes, err)
	}
	cfg.NetworkAccess, cfg.LocalDevelopmentNetwork = false, false
	if yes, _ := cfg.DevelopmentEnvironmentAccepted(); !yes {
		t.Fatal("tightening asked again")
	}
	cfg.AdditionalDirectories = []string{t.TempDir()}
	if yes, _ := cfg.DevelopmentEnvironmentAccepted(); yes {
		t.Fatal("new writable root inherited consent")
	}
	if err := cfg.AcceptDevelopmentEnvironment(); err != nil {
		t.Fatal(err)
	}
	cfg.LocalDevelopmentNetwork = true
	if yes, _ := cfg.DevelopmentEnvironmentAccepted(); yes {
		t.Fatal("new TCP capability inherited consent")
	}
	path, _ := cfg.developmentBoundaryPath()
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.DevelopmentEnvironmentAccepted(); err == nil {
		t.Fatal("corrupt consent accepted")
	}
}
