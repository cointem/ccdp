package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelObjectSchema(t *testing.T) {
	t.Setenv("TEST_ROUTER_KEY", "test-secret")
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"model":"first/same","providers":{"first":{"base_url":"https://first.example/v1","api_key_env":"TEST_ROUTER_KEY","wire_api":"responses","models":{"same":{"context_window":1000000,"max_output_tokens":16000,"reasoning_effort":"high","pricing":{"input_per_million":2,"output_per_million":4}}}},"second":{"base_url":"https://second.example/v1","wire_api":"chat","models":{"same":{"context_window":128000},"org/model":{}}}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveProvider("first/same"); got.ID != "first" || got.APIKey != "test-secret" || got.Wire != "responses" {
		t.Fatal("wrong route")
	}
	if cfg.ResolveProvider("second/same").ID != "second" || cfg.APIModelFor("second/org/model") != "org/model" {
		t.Fatal("provider identity lost")
	}
	if cfg.ContextWindowFor("first/same") != 1000000 || cfg.ContextWindowFor("second/same") != 128000 {
		t.Fatal("context crossed providers")
	}
	if cfg.MaxOutputTokensFor("first/same") != 16000 || cfg.ReasoningEffortFor("first/same") != "high" {
		t.Fatal("model options lost")
	}
	if cfg.CostFor("first/same", 1000000, 1000000) != 6 {
		t.Fatal("model pricing lost")
	}
	for _, model := range []string{"same", "missing/same", "first/missing"} {
		if cfg.ValidateModelSelection(model) == nil {
			t.Fatalf("accepted %s", model)
		}
	}
	clone := cfg.Clone()
	v := clone.Providers["first"].ModelConfigs["same"]
	v.Pricing.Input = 99
	if cfg.ModelConfigFor("first/same").Pricing.Input != 2 {
		t.Fatal("mutable pricing shared")
	}
	encoded, _ := json.Marshal(cfg.Providers)
	if strings.Contains(string(encoded), "test-secret") {
		t.Fatal("env credential materialized in config")
	}
}

func TestRejectOldModelConfiguration(t *testing.T) {
	for _, data := range []string{
		`{"max_reply_tokens":8192}`, `{"api_key":"old"}`, `{"base_url":"https://example.com"}`, `{"context_window":1000000}`, `{"pricing":{}}`,
		`{"providers":{"p":{"models":["m"]}}}`,
		`{"providers":{"p":{"models":{"m":{}},"context_windows":{"m":1000000}}}}`,
		`{"model":"p/m","providers":{"p":{"base_url":"https://example.com","wire_api":"respinses","models":{"m":{}}}}}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(data), 0600)
		if _, err := LoadFrom(path); err == nil {
			t.Fatalf("accepted obsolete/invalid config: %s", data)
		}
	}
}

func TestAnthropicWireAccepted(t *testing.T) {
	for _, wire := range []string{"anthropic", "anthropic-messages"} {
		data := `{"model":"c/m","providers":{"c":{"base_url":"https://api.anthropic.com","wire_api":"` + wire + `","models":{"m":{}}}}}`
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(data), 0600)
		cfg, err := LoadFrom(path)
		if err != nil {
			t.Fatalf("wire_api %q rejected: %v", wire, err)
		}
		if got := cfg.WireFor("c/m"); got != "anthropic" {
			t.Fatalf("wire_api %q normalized = %q, want anthropic", wire, got)
		}
	}
}

func TestPerModelOutputDefaultDoesNotInheritAnotherModelCap(t *testing.T) {
	cfg := Default()
	cfg.Providers = map[string]ProviderConfig{"p": {ModelConfigs: map[string]ModelConfig{
		"a": {MaxOutputTokens: 64000}, "b": {}, "c": {MaxOutputTokens: 0},
	}}}
	for model, want := range map[string]int{"p/a": 64000, "p/b": 32000, "p/c": 32000} {
		if got := cfg.MaxOutputTokensFor(model); got != want {
			t.Fatalf("%s: got %d want %d", model, got, want)
		}
	}
}
