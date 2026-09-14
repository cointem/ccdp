package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationConfigPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"reasoning_effort":"high","verbosity":"low"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEffort != "high" || cfg.Verbosity != "low" {
		t.Fatalf("file values lost: %q %q", cfg.ReasoningEffort, cfg.Verbosity)
	}
	t.Setenv("CCDP_REASONING_EFFORT", "max")
	t.Setenv("CCDP_VERBOSITY", "")
	cfg, err = LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEffort != "max" || cfg.Verbosity != "" {
		t.Fatal("environment presence override lost")
	}
	if err := cfg.ApplyCLIOverride("reasoning_effort", "low"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyCLIOverride("verbosity", "high"); err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEffort != "low" || cfg.Verbosity != "high" {
		t.Fatal("CLI override lost")
	}
	cfg.Verbosity = "loud"
	if cfg.Validate() == nil {
		t.Fatal("invalid verbosity accepted")
	}
	t.Setenv("CCDP_REASONING_EFFORT", "invalid")
	if _, err := LoadFrom(path); err == nil {
		t.Fatal("invalid environment accepted")
	}
}
