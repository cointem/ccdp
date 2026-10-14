package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestSharedAttemptImmediateIdempotentAndRecovered(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.EnableGuardian = config.BoolPtr(false)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	id := a.SessionID()
	fact := session.Attempt{AttemptID: "descendant-attempt", RequestID: "descendant-request", SessionID: "descendant-session", Provider: "provider", Model: cfg.Model, Outcome: "success", Usage: session.Usage{InputTokens: 100, OutputTokens: 50, CachedTokens: 20, TotalTokens: 150, TurnCount: 1}}
	// CacheStats is protocol-owned; use a real attempt's cache shape below.
	fact.Usage.Cache.Tracking = true
	fact.Usage.Cache.InputTokens = 100
	fact.Usage.Cache.CachedTokens = 20
	fact.Usage.Cache.ReportedInputTokens = 100
	for i := 0; i < 2; i++ {
		if err := a.accountSharedAttempt(fact); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.Usage(); got.InputTokens != 100 || got.CachedTokens != 20 || got.Cache.Tracking || got.Cache.InputTokens != 0 || got.Cache.CachedTokens != 0 {
		t.Fatalf("bad immediate usage: %+v", got)
	}
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resume(&cfg, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.accountSharedAttempt(fact); err != nil {
		t.Fatal(err)
	}
	if got := b.Usage(); got.InputTokens != 100 {
		t.Fatalf("replay counted twice: %+v", got)
	}
}

func TestPreparedCallPreservesFrozenRequest(t *testing.T) {
	a, _, cfg := newJournalAgent(t, true)

	p := &childRuntimeTestProvider{name: "frozen", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if req.Messages[0].Content != "original" {
			return llm.StreamResult{}, errors.New("request changed after preparation")
		}
		if req.Tools[0].Function.Parameters["type"] != "object" {
			return llm.StreamResult{}, errors.New("tool schema changed after preparation")
		}
		return llm.StreamResult{PromptTokens: 5, CompletionTok: 1}, nil
	}}
	req := journalRequest()
	req.Messages = []llm.ChatMessage{{Role: "user", Content: "original"}}
	call, err := a.prepareProviderCall(modelBinding{client: p, provider: p.Name(), model: cfg.Model}, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Content = "mutated"
	req.Tools[0].Function.Parameters["type"] = "mutated"
	_, err = a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: *cfg}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSharedTokenUsageTwoChildrenAndRootFinalAndResume(t *testing.T) {
	started := make(chan int, 3)
	release := make(chan struct{})
	p := &childRuntimeTestProvider{name: "shared-live", stream: func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if req.MaxTokens == nil || *req.MaxTokens <= 0 {
			return llm.StreamResult{}, errors.New("request lacks bounded output")
		}
		started <- *req.MaxTokens
		select {
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		case <-release:
		}
		return llm.StreamResult{Text: "done", FinishReason: "stop", PromptTokens: 1000, CompletionTok: 10}, nil
	}}
	root, cfg := newChildRuntimeTestAgent(t, p)
	runs := make([]*managedRun, 0, 2)
	for i := 0; i < 2; i++ {
		r, err := root.services.supervisor.launch(root, context.Background(), childTask{Description: "inspect", Role: "explorer"}, childPurposeTask, "budget")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	req := journalRequest()
	req.Model = cfg.Model
	req.MaxTokens = intPtr(cfg.MaxOutputTokensFor(cfg.Model))
	call, err := root.prepareProviderCall(modelBinding{client: p, provider: p.Name(), model: cfg.Model}, req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := root.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: root.configSnapshot()}, nil, nil)
		done <- err
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent requests did not start")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		<-r.done
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	want := 3030
	if got := root.Usage().InputTokens + root.Usage().OutputTokens; got != want {
		t.Fatalf("immediate/final total counted twice: %d want %d", got, want)
	}
	id := root.SessionID()
	if err := root.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(&cfg, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if got := resumed.Usage().InputTokens + resumed.Usage().OutputTokens; got != want {
		t.Fatalf("resume total changed: %d", got)
	}
}

func TestResumeReconcilesUncopiedDescendantAttempt(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.EnableGuardian = config.BoolPtr(false)
	root, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	childCfg := cfg.Clone()
	childCfg.SessionID = ""
	childCfg.Model = cfg.Model
	child, err := testNewWithOptions(t, &childCfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	req := journalRequest()
	req.Model = cfg.Model
	attempt, err := child.recordPreparedRequest(context.Background(), "task", "provider", "", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 100, CompletionTok: 10}, nil); err != nil {
		t.Fatal(err)
	}
	want := child.Usage().InputTokens + child.Usage().OutputTokens
	ref := session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{SessionID: protocol.SessionID(child.SessionID()), ParentSessionID: protocol.SessionID(root.SessionID()), RootSessionID: protocol.SessionID(root.SessionID()), DelegationID: "recovery-delegate", Purpose: "task", Run: protocol.RunView{ID: "recovery-run", Status: "unknown", WaitPolicy: "join"}}}
	if _, err := root.persistenceHandle().CommitEvents("recovery-reference", ref); err != nil {
		t.Fatal(err)
	}
	id := root.SessionID()
	if root.Usage().InputTokens != 0 || root.Usage().OutputTokens != 0 {
		t.Fatal("fixture copied child usage prematurely")
	}
	if err := child.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		snap, err := LoadSession(cfg.SessionDir, id)
		if err != nil {
			t.Fatal(err)
		}
		resumed, err := Resume(&cfg, snap)
		if err != nil {
			t.Fatal(err)
		}
		if got := resumed.Usage().InputTokens + resumed.Usage().OutputTokens; got != want {
			t.Fatalf("resume %d tokens %v want %v", i, got, want)
		}
		if err := resumed.CloseContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
