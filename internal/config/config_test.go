package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/hooks"
)

func TestLoadFromEnvironmentOverridesFile(t *testing.T) {
	t.Setenv("CCDP_API_KEY", "env-key")
	t.Setenv("CCDP_BASE_URL", "https://env.example/v1")
	t.Setenv("CCDP_MODEL", "env-model")
	t.Setenv("CCDP_PERMISSION_MODE", "bypassPermissions")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
  "api_key":"file-key",
  "base_url":"https://file.example/v1",
  "model":"file-model",
  "permission_mode":"default"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "env-key" || cfg.BaseURL != "https://env.example/v1" ||
		cfg.Model != "env-model" || cfg.PermissionMode != "bypassPermissions" {
		t.Fatalf("environment did not win over file: %+v", cfg)
	}
}

func TestSavePermissionRulesUsesSourcePathWithoutPersistingRuntimeConfig(t *testing.T) {
	t.Setenv("CCDP_API_KEY", "environment-secret")
	path := filepath.Join(t.TempDir(), "custom.json")
	if err := os.WriteFile(path, []byte(`{"model":"saved-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AlwaysAllow = []string{"Bash:go test *"}
	if err := cfg.SavePermissionRules(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || containsString(string(data), "environment-secret") {
		t.Fatalf("environment credential was persisted: %s", data)
	}
	var saved Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "saved-model" || len(saved.AlwaysAllow) != 1 {
		t.Fatalf("custom config was not updated: %+v", saved)
	}
	if saved.BaseURL != "" || saved.SystemPrompt != "" {
		t.Fatalf("resolved runtime defaults leaked into config: %+v", saved)
	}
}

func containsString(s, sub string) bool {
	return strings.Contains(s, sub)
}

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

func TestTriStateBoolMerge(t *testing.T) {
	// An explicit false must survive the merge over the true default.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"enable_web_tools": false,
		"enable_guardian": true,
		"enable_memory": false
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.WebToolsEnabled() {
		t.Error("enable_web_tools:false was ignored by the merge")
	}
	if !cfg.GuardianEnabled() {
		t.Error("enable_guardian:true was ignored by the merge")
	}
	if cfg.MemoryEnabled() {
		t.Error("enable_memory:false was ignored by the merge")
	}

	// Unset keys fall back to the defaults (web on, guardian/memory off).
	cfg2, err := LoadFrom(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg2.WebToolsEnabled() || cfg2.GuardianEnabled() || cfg2.MemoryEnabled() {
		t.Errorf("defaults wrong: web=%v guardian=%v memory=%v",
			cfg2.WebToolsEnabled(), cfg2.GuardianEnabled(), cfg2.MemoryEnabled())
	}
}

func TestLoadFromPreservesExplicitZeroAndEmptyValuesAndSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"max_turns": 0,
		"max_budget_usd": 0,
		"always_allow": [],
		"always_deny": [],
		"tools": [],
		"additional_directories": [],
		"enable_web_tools": false
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurns != 0 || cfg.MaxBudgetUSD != 0 || cfg.WebToolsEnabled() {
		t.Fatalf("explicit zero/false values were not retained: %+v", cfg)
	}
	for _, field := range []string{"max_turns", "max_budget_usd", "always_allow", "always_deny", "tools", "additional_directories", "enable_web_tools"} {
		if !cfg.IsExplicit(field) || cfg.SourceOf(field) != SourceUserFile {
			t.Fatalf("field %q provenance = %+v", field, cfg.SourceReport().Fields[field])
		}
	}
	if cfg.SourceOf("model") != SourceDefault || cfg.IsExplicit("model") {
		t.Fatalf("unset field model provenance = %+v", cfg.SourceReport().Fields["model"])
	}
}

func TestProjectExecutableTrustRequiresExactFingerprint(t *testing.T) {
	root := t.TempDir()
	settingsDir := filepath.Join(root, ".ccdp")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{
		"hooks":{"PreToolUse":["echo hook"]},
		"tools":[{"name":"x","command":"echo x"}],
		"mcp_servers":{"s":{"command":"echo"}},
		"always_deny":["Bash:rm *"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadProjectSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if proj.SourceReport().ProjectTrusted || proj.SourceReport().ProjectFingerprint == "" {
		t.Fatalf("default project trust = %+v", proj.SourceReport())
	}
	dst := Default()
	dst.AlwaysDeny = []string{"Write:protected"}
	ApplyProjectSettings(&dst, &proj)
	if len(dst.Hooks) != 0 || len(dst.Tools) != 0 || len(dst.MCPServers) != 0 {
		t.Fatalf("untrusted executable settings applied: hooks=%v tools=%v mcp=%v", dst.Hooks, dst.Tools, dst.MCPServers)
	}
	if !containsString(strings.Join(dst.AlwaysDeny, "\n"), "Bash:rm *") || !containsString(strings.Join(dst.AlwaysDeny, "\n"), "Write:protected") {
		t.Fatalf("project deny was not cumulative: %v", dst.AlwaysDeny)
	}

	trust := NewTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	if _, err := trust.AuthorizeProject(root, proj); err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadProjectSettingsTrusted(root, trust)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted.SourceReport().ProjectTrusted {
		t.Fatalf("explicit trust not recognized: %+v", trusted.SourceReport())
	}
	ApplyProjectSettings(&dst, &trusted)
	if len(dst.Hooks) != 1 || len(dst.Tools) != 1 || len(dst.MCPServers) != 1 {
		t.Fatalf("trusted executable settings not applied: hooks=%v tools=%v mcp=%v", dst.Hooks, dst.Tools, dst.MCPServers)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"tools":[{"name":"x","command":"touch changed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := LoadProjectSettingsTrusted(root, trust)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SourceReport().ProjectTrusted || len(changed.Tools) == 0 {
		t.Fatalf("changed executable config retained trust: %+v", changed.SourceReport())
	}
}

func TestLoadFromRejectsMalformedEnvironmentOverride(t *testing.T) {
	t.Setenv("CCDP_MAX_TURNS", "not-an-int")
	if _, err := LoadFrom(filepath.Join(t.TempDir(), "config.json")); err == nil {
		t.Fatal("malformed environment override was silently ignored")
	}
}

func TestProjectFingerprintIncludesStructuredHookFields(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	cfg.Hooks = hooks.Config{"PreToolUse": {{Matcher: "Read", Command: "echo", Timeout: 1}}}
	first, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hooks["PreToolUse"][0].Timeout = 2
	second, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("hook timeout change did not invalidate project fingerprint")
	}
	cfg.Hooks["PreToolUse"][0].Matcher = "Write"
	third, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatal("hook matcher change did not invalidate project fingerprint")
	}
}

func TestProjectTrustInvalidatesPrivilegeChanges(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	first, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AlwaysAllow = []string{"Bash:git push *"}
	second, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("adding a project allow rule retained executable trust fingerprint")
	}
	cfg.EnableWebTools = BoolPtr(false)
	third, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatal("changing project web capability retained trust fingerprint")
	}
	cfg.AdditionalDirectories = []string{filepath.Join(root, "outside")}
	fourth, err := ProjectExecutableFingerprint(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if third == fourth {
		t.Fatal("adding project sandbox directory retained trust fingerprint")
	}
}

func TestCloneDeepCopiesOptionalPointersAndNestedSchemas(t *testing.T) {
	guardian, memory, web := true, false, true
	cfg := Default()
	cfg.EnableGuardian, cfg.EnableMemory, cfg.EnableWebTools = &guardian, &memory, &web
	cfg.Tools = []ToolSpec{{InputSchema: map[string]any{
		"properties": map[string]any{"items": []any{map[string]any{"type": "string"}}},
	}}}
	clone := cfg.Clone()
	*clone.EnableGuardian = false
	*clone.EnableMemory = true
	*clone.EnableWebTools = false
	clone.Tools[0].InputSchema["properties"].(map[string]any)["items"].([]any)[0].(map[string]any)["type"] = "integer"
	if !*cfg.EnableGuardian || *cfg.EnableMemory || !*cfg.EnableWebTools {
		t.Fatal("optional config pointers were aliased")
	}
	if got := cfg.Tools[0].InputSchema["properties"].(map[string]any)["items"].([]any)[0].(map[string]any)["type"]; got != "string" {
		t.Fatalf("nested schema was aliased: %v", got)
	}
}
