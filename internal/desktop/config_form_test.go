package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProviderFormUpdatesBindingsAndKeepsAdvancedFields(t *testing.T) {
	const source = `{
		"model":"hosted/org/small", "fallback_model":"hosted/org/small",
		"futureSetting":{"large":9007199254740993}, "max_turns":8,
		"providers":{"hosted":{"name":"My host", "base_url":"https://api.example/v1",
			"api_key":"fixture-private", "wire_api":"openai-responses",
			"models":{"org/small":{"context_window":null,"max_output_tokens":0},"other":null}}}
	}`
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.dirty() || !slices.Equal(f.modelOptions(), []string{"hosted/org/small", "hosted/other"}) {
		t.Fatal("initial form does not reflect the stored models")
	}
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	if f.file.Text != source {
		t.Fatal("unchanged file was reformatted")
	}
	p := f.Providers[0]
	p.ID, p.Models[0].ID = "remote", "org/large"
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	if f.dirty() || f.Model != "remote/org/large" || f.FallbackModel != f.Model {
		t.Fatal("bindings did not follow rename")
	}
	var doc struct {
		Future    struct{ Large int64 } `json:"futureSetting"`
		MaxTurns  int                   `json:"max_turns"`
		Providers map[string]struct {
			Name   string
			Key    string `json:"api_key"`
			Wire   string `json:"wire_api"`
			Models map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(f.file.Text), &doc); err != nil {
		t.Fatal(err)
	}
	stored := doc.Providers["remote"]
	if doc.Future.Large != 9007199254740993 || doc.MaxTurns != 8 || stored.Name != "My host" || stored.Wire != "openai-responses" || stored.Key != "fixture-private" {
		t.Fatal("provider edit changed unrelated configuration")
	}
	if string(stored.Models["other"]) != "null" {
		t.Fatal("rewrote an untouched null model")
	}
	var limits map[string]json.RawMessage
	if err := json.Unmarshal(stored.Models["org/large"], &limits); err != nil {
		t.Fatal(err)
	}
	if string(limits["context_window"]) != "null" || string(limits["max_output_tokens"]) != "0" {
		t.Fatal("rewrote optional model limits")
	}
	p.Models[0].ContextWindow = "8192"
	p.Models[0].MaxOutputTokens = "512"
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	again, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Providers[0].Models[0].MaxOutputTokens != "512" {
		t.Fatal("repeated save lost model limit")
	}
}

func TestNewProviderFormValidatesBeforeCreatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.json")
	form, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerForm{ID: "local", BaseURL: "http://localhost:8080", WireAPI: "chat", APIKeyEnv: "CCDP_TEST_FORM_KEY", Models: []*modelForm{{ID: "a", ContextWindow: "bad"}}}
	form.Providers = append(form.Providers, provider)
	if err := form.save(path); err == nil {
		t.Fatal("noninteger limit accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file created despite invalid settings")
	}
	t.Setenv("CCDP_TEST_FORM_KEY", "only-in-process")
	provider.Models[0].ContextWindow = "40000"
	if err := form.save(path); err != nil {
		t.Fatal(err)
	}
	if form.Model != "local/a" || strings.Contains(form.file.Text, "only-in-process") {
		t.Fatal("wrong default model or resolved an environment key into the file")
	}
	provider.Models = append(provider.Models, &modelForm{ID: "a"})
	if err := form.save(path); err == nil {
		t.Fatal("duplicate model silently overwritten")
	}
	provider.Models = provider.Models[:1]
	form.Providers = nil
	if err := form.save(path); err != nil {
		t.Fatal(err)
	}
	if form.Model != "" {
		t.Fatal("left a model selection for a removed provider")
	}
}

func TestProviderFormSelectionsFollowRowsThroughRenamesAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const source = `{"model":"p/a","providers":{"p":{"base_url":"https://example.com","wire_api":"chat","models":{"a":{},"b":{}}}}}`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	form, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	p := form.Providers[0]
	newModel := &modelForm{ID: "c"}
	p.Models = append(p.Models, newModel)
	form.selectDefaultModel("p/b")
	form.selectFallbackModel("p/c")
	p.ID, p.Models[1].ID, newModel.ID = "remote", "main", "org/alternate"
	check := func(wantMain, wantFallback string) {
		t.Helper()
		if form.defaultModel() != wantMain || form.fallbackModel() != wantFallback {
			t.Fatalf("picker shows (%q, %q), want (%q, %q)", form.defaultModel(), form.fallbackModel(), wantMain, wantFallback)
		}
		if err := form.save(path); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadConfigForm(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Model != wantMain || loaded.FallbackModel != wantFallback {
			t.Fatalf("saved (%q, %q), want (%q, %q)", loaded.Model, loaded.FallbackModel, wantMain, wantFallback)
		}
	}
	check("remote/main", "remote/org/alternate")
	p.Models[1].ID = "main-v2"
	check("remote/main-v2", "remote/org/alternate")
	p.Models = p.Models[:1]
	check("remote/a", "")
}

func TestProviderFormKeepsModelNamesWithBuiltInEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const source = `{"model":"custom","fallback_model":"custom-backup"}`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	form, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	form.PermissionMode = "manual"
	if err := form.save(path); err != nil {
		t.Fatal(err)
	}
	if form.defaultModel() != "custom" || form.fallbackModel() != "custom-backup" {
		t.Fatal("editing permissions changed model names for the built-in endpoint")
	}
}
