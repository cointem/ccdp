package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
)

func TestProjectInputAuthorizationRejectsAliasesAndAllowsRetry(t *testing.T) {
	const secret = "DENIED_INPUT_TEST_SECRET"
	for _, tc := range []struct {
		name, path, content, errorPrefix string
	}{
		{
			name: "skill", path: ".ccdp/skills/denied-input/SKILL.md",
			content:     "---\nname: denied-input\ndescription: " + secret + "\n---\n" + secret,
			errorPrefix: "load skills",
		},
		{
			name: "instructions", path: "AGENTS.md", content: secret,
			errorPrefix: "load project instructions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := isolatedTestConfig(t, "input-authorization-model")
			cfg.NoSessionPersistence = false
			cfg.MaxTurns = 3
			private := t.TempDir()
			cfg.DisallowedDirectories = []string{private}
			source := filepath.Join(private, "secret.md")
			if err := os.WriteFile(source, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(cfg.Workspace, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(source, alias); err != nil {
				t.Fatal(err)
			}

			provider := &formalCompletionProvider{}
			models := plugin.NewModelRegistry()
			models.Register(provider)
			models.Route(cfg.Model, provider.Name())
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
			if tc.name == "skill" && len(a.services.skills.All()) != 0 {
				t.Fatal("constructor cached a skill from the denied alias")
			}
			if strings.Contains(a.services.skills.SkillsSection(), secret) {
				t.Fatal("constructor exposed the denied skill description")
			}

			formalCompletionTurn(t, a, "denied-input", "inspect this workspace")
			if provider.requestsCount() != 0 {
				t.Fatalf("provider called with unauthorized input: %d", provider.requestsCount())
			}
			view, err := a.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnFailed ||
				!strings.Contains(view.LastTurn.Error, tc.errorPrefix) ||
				!strings.Contains(view.LastTurn.Error, "denied") {
				t.Fatalf("input rejection did not fail this turn: %+v", view.LastTurn)
			}
			if err := a.persistenceFailure(); err != nil {
				t.Fatalf("input authorization failure poisoned persistence: %v", err)
			}

			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			formalCompletionTurn(t, a, "fixed-input", "retry after removing the denied alias")
			if provider.requestsCount() != 1 {
				t.Fatalf("provider requests after retry=%d, want 1", provider.requestsCount())
			}
			view, err = a.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnSucceeded {
				t.Fatalf("fixed input did not recover the next turn: %+v", view.LastTurn)
			}
			if err := a.persistenceFailure(); err != nil {
				t.Fatalf("retry left persistence unusable: %v", err)
			}
			provider.mu.Lock()
			requests, err := json.Marshal(provider.requests)
			provider.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(requests), secret) {
				t.Fatal("denied input leaked into the retried provider request")
			}
		})
	}
}
