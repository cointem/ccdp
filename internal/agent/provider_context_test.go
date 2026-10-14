package agent

import (
	"fmt"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
)

func TestProviderContextBudgetAndAdapterRefresh(t *testing.T) {
	cfg := isolatedTestConfig(t, "base-model")
	cfg.ContextWindow = 200000
	cfg.Providers = map[string]config.ProviderConfig{"other": {Models: []string{"step-5-preview", "small-model"}, ContextWindow: 800000, ContextWindows: map[string]int{"step-5-preview": 1000000, "small-model": 128000}}}
	registry := plugin.NewModelRegistry()
	for _, tc := range []struct {
		model  string
		window int
	}{{"step-5-preview", 1000000}, {"base-model", 200000}, {"small-model", 128000}, {"step-5-preview", 1200000}} {
		if tc.window == 1200000 {
			p := cfg.Providers["other"]
			p.ContextWindows[tc.model] = tc.window
			cfg.Providers["other"] = p
		}
		route, err := resolveProviderForModel(cfg, registry, tc.model)
		if err != nil {
			t.Fatal(err)
		}
		caps, _ := llm.ProviderCapabilitiesOf(route.client)
		if caps.ContextWindow != tc.window {
			t.Fatalf("%s: %d != %d", tc.model, caps.ContextWindow, tc.window)
		}
	}
	if cfg.ContextWindow != 200000 {
		t.Fatal("mutated fallback")
	}
}

func TestModelSwitchUpdatesContextDisplayWithoutChangingFallback(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	a.services.cfg.Providers = map[string]config.ProviderConfig{"other": {Models: []string{"step-5-preview", "small-model"}, ContextWindow: 800000, ContextWindows: map[string]int{"step-5-preview": 1000000, "small-model": 128000}}}
	a.services.baseCfg.Providers = a.services.cfg.Providers
	fallback := a.services.cfg.ContextWindow
	original := a.services.cfg.Model
	a.mu.Unlock()
	for i, model := range []string{"step-5-preview", original} {
		cmd := protocol.NewSetModel(protocol.CommandID([]string{"large", "back"}[i]), protocol.SessionID(a.SessionID()), model)
		if r := a.applyCommand(cmd); r.Rejected() {
			t.Fatal(r.Error)
		}
		want := fallback
		if i == 0 {
			want = 1000000
		}
		if got := a.settingsSnapshotLocked().ContextWindow; got != want {
			t.Fatalf("display %d != %d", got, want)
		}
		saved := a.sessionSettingsLocked()
		if saved.ContextWindow != fallback || saved.EffectiveContextWindow != want {
			t.Fatalf("persisted windows: %d/%d", saved.ContextWindow, saved.EffectiveContextWindow)
		}
	}
}

func TestConfiguredModelRemainsInCatalogAfterSwitch(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	original := a.services.cfg.Model
	a.services.cfg.Providers = map[string]config.ProviderConfig{"other": {Models: []string{"other-model"}}}
	a.services.baseCfg.Providers = a.services.cfg.Providers
	a.mu.Unlock()
	for i, model := range []string{"other-model", original, "other-model"} {
		cmd := protocol.NewSetModel(protocol.CommandID(fmt.Sprintf("catalog-switch-%d", i)), protocol.SessionID(a.SessionID()), model)
		if r := a.applyCommand(cmd); r.Rejected() {
			t.Fatal(r.Error)
		}
		catalog := a.catalogSnapshotLocked()
		seen := map[string]bool{}
		for _, provider := range catalog.Providers {
			for _, name := range provider.Models {
				seen[name] = true
			}
		}
		if !seen[original] || !seen["other-model"] {
			t.Fatalf("switch to %s lost choices: %v", model, seen)
		}
	}
}

func TestQualifiedProviderSwitchKeepsDistinctModels(t *testing.T) {
	cfg := isolatedTestConfig(t, "first/same")
	cfg.Providers = map[string]config.ProviderConfig{
		"first":  {BaseURL: "https://first.example/v1", WireAPI: "responses", ModelConfigs: map[string]config.ModelConfig{"same": {ContextWindow: 1000000}}},
		"second": {BaseURL: "https://second.example/v1", WireAPI: "chat", ModelConfigs: map[string]config.ModelConfig{"same": {ContextWindow: 128000}}},
	}
	a, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i, model := range []string{"second/same", "first/same"} {
		r := a.applyCommand(protocol.NewSetModel(protocol.CommandID(fmt.Sprintf("qualified-%d", i)), protocol.SessionID(a.SessionID()), model))
		if r.Rejected() {
			t.Fatal(r.Error)
		}
		if a.modelsState.activeBinding.client.Name() != model {
			t.Fatal("registry alias collision")
		}
		caps, _ := llm.ProviderCapabilitiesOf(a.modelsState.activeBinding.client)
		if caps.ContextWindow != cfg.ContextWindowFor(model) {
			t.Fatal("wrong context")
		}
		seen := map[string]bool{}
		for _, p := range a.catalogSnapshotLocked().Providers {
			for _, m := range p.Models {
				seen[m] = true
			}
		}
		if len(seen) != 2 || !seen["first/same"] || !seen["second/same"] {
			t.Fatalf("catalog %v", seen)
		}
	}
}
