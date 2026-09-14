package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
)

func TestNewSessionIDUniqueConcurrent(t *testing.T) {
	const (
		workers   = 32
		perWorker = 512
	)
	ids := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ids <- newSessionID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, workers*perWorker)
	for id := range ids {
		if err := validateSessionID(id); err != nil {
			t.Fatalf("generated invalid session id %q: %v", id, err)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("generated duplicate session id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("generated %d unique ids, want %d", len(seen), workers*perWorker)
	}
}

type sessionIDTestProvider struct{}

func (sessionIDTestProvider) Name() string { return "session-id-test-provider" }

func (sessionIDTestProvider) Stream(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	return llm.StreamResult{Text: "ok"}, nil
}

func TestNewConcurrentAgentsHaveUniqueSessionIDs(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
	cfg.Model = "session-id-test-model"
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.NoSessionPersistence = true
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(false)
	models := plugin.NewModelRegistry()
	provider := sessionIDTestProvider{}
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())

	const workers = 32
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := NewWithOptions(&cfg, nil, Options{
				EffectiveConfigFrozen: true,
				ProviderRegistry:      models,
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- a.SessionID()
			a.Close()
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	seen := make(map[string]struct{}, workers)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("concurrent agents shared session id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("constructed %d agents, got %d unique session ids", workers, len(seen))
	}
}
