package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
)

func TestMarkdownImagesRespectReadPolicy(t *testing.T) {
	cfg := isolatedTestConfig(t, "image-authorization-model")
	private := filepath.Join(cfg.Workspace, "private")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.DisallowedDirectories = []string{private}
	secret := inputPNG(t, 123)
	source := filepath.Join(private, "secret.png")
	if err := os.WriteFile(source, secret.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(cfg.Workspace, "alias.png")); err != nil {
		t.Fatal(err)
	}
	allowed := inputPNG(t, 45)
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "allowed.png"), allowed.Data, 0o600); err != nil {
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

	for _, path := range []string{"private/secret.png", "alias.png"} {
		if _, err := a.currentSandbox().ResolveRead(path); err == nil {
			t.Fatalf("test setup allowed denied path %q", path)
		}
		cmd := protocol.NewSubmitInput(protocol.CommandID(path), protocol.SessionID(a.SessionID()), protocol.InputID(path), "inspect ![image]("+path+")", protocol.InputSteer)
		receipt, err := a.Submit(context.Background(), cmd)
		if err != nil || !receipt.Rejected() {
			t.Fatalf("denied attachment admission: receipt=%+v err=%v", receipt, err)
		}
	}
	if provider.requestsCount() != 0 {
		t.Fatal("denied image admission reached the provider")
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("rejected attachment poisoned persistence: %v", err)
	}

	// Collaboration/legacy messages are expanded at request construction.
	// They must use the frozen step policy too.
	cmd := protocol.NewSubmitInput("legacy-image", protocol.SessionID(a.SessionID()), "legacy-image", "reference ![image](alias.png)", protocol.InputMessage)
	if receipt, err := a.Submit(context.Background(), cmd); err != nil || receipt.Rejected() {
		t.Fatalf("message admission: receipt=%+v err=%v", receipt, err)
	}
	formalCompletionTurn(t, a, "allowed-image", "inspect ![image](allowed.png)")
	provider.mu.Lock()
	requests, err := json.Marshal(provider.requests)
	provider.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requests), base64.StdEncoding.EncodeToString(secret.Data)) {
		t.Fatal("denied image bytes leaked into a provider request")
	}
	if !strings.Contains(string(requests), base64.StdEncoding.EncodeToString(allowed.Data)) {
		t.Fatal("authorized image did not reach the provider")
	}
}
