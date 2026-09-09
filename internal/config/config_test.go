package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointFor(t *testing.T) {
	cfg := Default()
	cfg.APIKey = "top-key"
	cfg.BaseURL = "https://default.example/v1"
	cfg.Providers = map[string]ProviderConfig{
		"z": {BaseURL: "https://z.example/v1", APIKey: "z-key", Models: []string{"m1"}},
		"a": {BaseURL: "https://a.example/v1", APIKey: "a-key", Models: []string{"m1", "m2"}},
	}

	// Sorted provider ids: a wins over z for m1.
	base, key := cfg.EndpointFor("m1")
	if base != "https://a.example/v1" || key != "a-key" {
		t.Errorf("m1 → %s/%s, want a provider", base, key)
	}
	base, key = cfg.EndpointFor("m2")
	if base != "https://a.example/v1" || key != "a-key" {
		t.Errorf("m2 → %s/%s, want a provider", base, key)
	}
	// Unknown model → top-level fallback.
	base, key = cfg.EndpointFor("nope")
	if base != "https://default.example/v1" || key != "top-key" {
		t.Errorf("nope → %s/%s, want fallback", base, key)
	}
}

func TestCostFor(t *testing.T) {
	cfg := Default()
	cfg.Model = "gpt-4o"
	cfg.Pricing = DefaultPricing()
	cost := cfg.CostFor("gpt-4o", 1_000_000, 500_000)
	if cost != 2.5+5.0 {
		t.Errorf("cost = %v, want 7.5", cost)
	}
	// Unknown model falls back to cfg.Model pricing.
	cost = cfg.CostFor("weird-model", 1_000_000, 0)
	if cost != 2.5 {
		t.Errorf("fallback cost = %v, want 2.5", cost)
	}
}

func TestLoadFromMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"base_url": "https://custom.example/v1",
		"model": "custom-model",
		"permission_mode": "acceptEdits",
		"max_budget_usd": 2.5,
		"tools": [{"name": "fmt", "description": "fmt", "command": "gofmt"}]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.BaseURL != "https://custom.example/v1" || cfg.Model != "custom-model" {
		t.Errorf("custom values not merged: %+v", cfg)
	}
	if cfg.PermissionMode != "acceptEdits" {
		t.Errorf("mode = %q", cfg.PermissionMode)
	}
	if cfg.MaxBudgetUSD != 2.5 {
		t.Errorf("max_budget = %v", cfg.MaxBudgetUSD)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "fmt" {
		t.Errorf("tools not merged: %+v", cfg.Tools)
	}
}

func TestLoadFromMissingFile(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should use defaults: %v", err)
	}
	if cfg.Model == "" || cfg.BaseURL == "" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}
