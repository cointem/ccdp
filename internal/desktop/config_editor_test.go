package desktop

import (
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

func TestConsecutiveSettingsSavesAndRetry(t *testing.T) {
	a, conv, callbacks := testApp(t)
	commands := make(chan protocol.Command, 3)
	results := make(chan error, 3)
	conv.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		commands <- cmd
		return protocol.Receipt{Status: protocol.ReceiptApplied}, <-results
	}}
	cfg := a.host.settings.Load().Clone()
	cfg.PermissionMode = "manual"
	a.applySavedConfig(cfg)
	a.applySavedConfig(cfg)
	if !conv.configPending || !conv.configApplying {
		t.Fatal("a second save should refresh again once the current refresh finishes")
	}
	results <- nil
	applyCallback(t, callbacks)
	results <- errors.New("test refresh failure")
	applyCallback(t, callbacks)
	if conv.configApplying || conv.configError == "" {
		t.Fatal("a failed refresh should be visible and retryable")
	}
	conv.draft = "保留草稿"
	a.send(conv)
	if conv.sending || conv.pending != nil {
		t.Fatal("submitted input with stale provider credentials")
	}
	a.refreshUserPreferences(conv)
	results <- nil
	applyCallback(t, callbacks)
	if conv.configApplying || conv.configError != "" || conv.configModeChanged || conv.draft != "保留草稿" {
		t.Fatal("retry did not finish cleanly")
	}
	for range 3 {
		cmd := <-commands
		if cmd.SessionID != conv.view.SessionID || cmd.Reload == nil || !cmd.Reload.UserPreferences || !cmd.Reload.PermissionModeChanged {
			t.Fatal("a coalesced save or its retry lost the permission change")
		}
	}
}

// This fixture may appear in the screenshot. Never use a real provider or key.
const exampleSettings = `{
	"model": "example/coder",
	"providers": {"example": {
		"base_url": "https://api.example.com/v1",
		"wire_api": "chat",
		"api_key": "fixture-only",
		"models": {"coder": {"context_window": 128000, "max_output_tokens": 8192}}
	}},
	"max_turns": 12
}`

func TestEditConfigInSettings(t *testing.T) {
	a, _, _ := testApp(t)
	a.configPath = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(a.configPath, []byte(exampleSettings), 0600); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	changeEndpoint := func(value string) {
		t.Helper()
		click("API 地址输入框")
		tt.Key(ui.Cmd, ui.KeyA)
		tt.Type(value)
	}
	endpoint := func(want string) {
		t.Helper()
		field, ok := tt.Access("API 地址输入框")
		if !ok || field.Value != want {
			t.Fatalf("endpoint field = %q, want %q", field.Value, want)
		}
	}
	click("设置")
	endpoint("https://api.example.com/v1")
	if key, ok := tt.Access("API 密钥输入框"); !ok || !key.Password || key.Value != "" {
		t.Fatal("a stored API key must be in a protected field")
	}
	if dir := os.Getenv("CCDP_DESKTOP_SCREENSHOTS"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "settings.png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}

	const edited = "https://new.example.com/v1"
	changeEndpoint(edited)
	click("通用")
	if field, ok := tt.AccessByRole(ui.RolePopUpButton, "默认模型"); !ok || field.Value != "example/coder" {
		t.Fatalf("default model control = %q", field.Value)
	}
	click("example")
	endpoint(edited)
	click("返回对话")
	click("继续编辑")
	endpoint(edited)
	click("保存配置")

	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		MaxTurns  int `json:"max_turns"`
		Providers map[string]struct {
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	provider := saved.Providers["example"]
	if provider.BaseURL != edited || provider.APIKey != "fixture-only" || saved.MaxTurns != 12 {
		t.Fatal("saving a single field lost persisted configuration")
	}
	if a.host.settings.Load().ResolveProvider("example/coder").BaseURL != edited {
		t.Fatal("the open desktop still uses its launch endpoint")
	}
	changeEndpoint("https://discard.example.com")
	click("返回对话")
	click("放弃更改")
	click("设置")
	endpoint(edited)

	tt.SetSize(780, 580)
	for _, label := range []string{"返回对话", "保存配置"} {
		button, ok := tt.AccessByRole(ui.RoleButton, label)
		box := button.Bounds
		if !ok || button.Offscreen || box.W < 20 || box.H < 20 || box.X < 0 || box.Y < 0 || box.X+box.W > 780 || box.Y+box.H > 580 {
			t.Fatalf("%s unavailable at minimum window size: %+v", label, box)
		}
	}
}

func TestSettingsCreateFirstProvider(t *testing.T) {
	a, _, _ := testApp(t)
	a.configPath = filepath.Join(t.TempDir(), "new", "config.json")
	tt := ui.NewTester(a.view, 1180, 900)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	click("设置")
	click("添加供应商")
	click("保存配置")
	if !tt.HasText("供应商标识不能为空") {
		t.Fatal("a blank provider should have a visible validation error")
	}
	if _, err := os.Stat(a.configPath); !os.IsNotExist(err) {
		t.Fatal("saving a blank provider should leave the new configuration file unwritten")
	}
	for _, field := range []struct{ label, text string }{
		{"供应商标识输入框", "local"},
		{"API 地址输入框", "http://127.0.0.1:11434/v1"},
		{"模型标识输入框", "coder"},
	} {
		click(field.label)
		tt.Key(ui.Cmd, ui.KeyA)
		tt.Type(field.text)
	}
	click("保存配置")
	form, err := loadConfigForm(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(form.Providers) != 1 || form.Model != "local/coder" || form.Providers[0].BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatal("a fresh installation could not create a provider and select its default model")
	}
}

func TestSettingsSaveProviderAndGeneralSelections(t *testing.T) {
	a, _, _ := testApp(t)
	a.workspace = t.TempDir()
	a.newConversation()
	a.configPath = filepath.Join(t.TempDir(), "config.json")
	const twoModels = `{
		"model": "example/coder",
		"providers": {"example": {
			"base_url": "https://api.example.com/v1", "wire_api": "chat",
			"api_key": "fixture-only", "models": {"coder": {}, "reader": {}}
		}}
	}`
	if err := os.WriteFile(a.configPath, []byte(twoModels), 0600); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(labels ...string) {
		t.Helper()
		for _, label := range labels {
			if err := tt.Click(label); err != nil {
				t.Fatal(err)
			}
		}
	}
	click("设置", "Chat Completions", "Responses", "环境变量", "密钥环境变量输入框")
	tt.Type("EXAMPLE_API_KEY")
	click("通用", "example/coder", "example/reader", "manual", "保存配置")
	form, err := loadConfigForm(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	provider := form.Providers[0]
	if provider.WireAPI != "responses" || provider.APIKeyEnv != "EXAMPLE_API_KEY" || provider.APIKey != "" {
		t.Error("selected protocol and credential source were not saved")
	}
	if form.Model != "example/reader" || form.PermissionMode != "manual" {
		t.Errorf("saved general settings = %q, %q", form.Model, form.PermissionMode)
	}
	draft := a.active()
	if draft.view.Settings.Model.Model != form.Model || draft.view.Settings.Permission.Mode != "manual" ||
		!slices.Contains(a.host.settings.Load().models, "example/coder") {
		t.Fatal("saving the settings form did not refresh the existing draft")
	}
}

func TestExaSearchSettings(t *testing.T) {
	t.Setenv("EXA_API_KEY", "environment-fixture")
	a, conv, callbacks := testApp(t)
	a.workspace, conv.view.Settings.Workspace = "", ""
	a.configPath = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(a.configPath, []byte(`{"max_turns":12}`), 0644); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(a.view, 1080, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	click("设置")
	click("MCP")
	applyCallback(t, callbacks)
	tt.Frame()
	click("exa")
	click("配置 MCP")
	const input = "Exa API 密钥输入框"
	if field, ok := tt.Access(input); !ok || !field.Password || field.Value != "" || a.pluginsPage.mcpEditor.key != "" {
		t.Fatal("expected an empty protected field, even with an environment key")
	}
	click(input)
	tt.Type("personal-fixture")
	click("保存 MCP")
	applyCallback(t, callbacks)
	tt.Frame()
	check := func(want string) {
		t.Helper()
		file, err := readConfigFile(a.configPath)
		if err != nil {
			t.Fatal(err)
		}
		if a.host.settings.Load().ExaAPIKey != want || strings.Contains(file.Text, "environment-fixture") ||
			strings.Contains(file.Text, "exa_api_key") != (want != "") || !strings.Contains(file.Text, "max_turns") || file.mode&0077 != 0 {
			t.Fatal("saved and effective search settings do not agree")
		}
	}
	check("personal-fixture")
	click("exa")
	click("配置 MCP")
	if field, ok := tt.Access(input); !ok || !field.Password || field.Value != "" {
		t.Fatal("reopened settings exposed the key")
	}
	click(input)
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Key(0, ui.KeyBackspace)
	click("保存 MCP")
	applyCallback(t, callbacks)
	tt.Frame()
	check("")
}
