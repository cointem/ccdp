package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func newJournalAgent(t *testing.T, memory bool) (*Agent, *sessionPersistence, *config.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.Model = "journal-model"
	cfg.Pricing = map[string]config.Pricing{"journal-model": {Input: 1, Output: 2}}
	cfg.NoSessionPersistence = memory
	p, err := openSessionPersistence(&cfg, "journal-session", time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		cfg:         &cfg,
		persistence: p,
		sessionID:   "journal-session",
		settingsRev: 7,
		contextRev:  9,
		turnSeq:     2,
		stepSeq:     4,
		perms:       permissions.NewManager(permissions.ModeDefault, permissions.Policy{}),
		evbus:       events.NewBus(),
		eventQueue:  make(chan Event, 8),
		eventDone:   make(chan struct{}),
		watchers:    make(map[uint64]*runtimeWatcher),
	}
	t.Cleanup(func() { _ = p.close() })
	return a, p, &cfg
}

func journalRequest() llm.CompletionRequest {
	return llm.CompletionRequest{
		Model: "journal-model",
		Messages: []llm.ChatMessage{{
			Role:    "user",
			Content: "Keep this ordinary prompt text: Authorization: prompt-value",
		}},
		Tools: []llm.ToolDef{{Type: "function", Function: llm.FuncDef{
			Name: "Read", Description: "read a file", Parameters: map[string]any{"type": "object"},
		}}},
		Stream: true,
	}
}

func TestRequestJournalReconstructsExactWireMemoryAndDisk(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(map[bool]string{true: "memory", false: "disk"}[memory], func(t *testing.T) {
			a, p, cfg := newJournalAgent(t, memory)
			req := journalRequest()
			ctx := withRequestJournalMetadata(context.Background(), requestJournalMetadata{
				Config: cfgValue(*cfg), SettingsRevision: 31, ContextRevision: 41,
				Turn: 12, Step: 13, HistoryLen: 17,
			})
			attempt, err := a.recordPreparedRequest(ctx, "main", "provider-a", "https://user:pass@example.test/v1/chat?api_key=url-secret", req)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Manifest.SessionID != "journal-session" || attempt.Manifest.TurnID != "turn-12" || attempt.Manifest.StepID != "step-13" || attempt.Manifest.SourceRevision != 31 {
				t.Fatalf("manifest metadata = %+v", attempt.Manifest)
			}
			if attempt.Manifest.Endpoint != "https://example.test/v1/chat" || strings.Contains(attempt.Manifest.Endpoint, "url-secret") {
				t.Fatalf("endpoint was not sanitized: %q", attempt.Manifest.Endpoint)
			}
			if attempt.Manifest.WireBody == nil || attempt.Manifest.WireBody.Hash != attempt.Manifest.Digest {
				t.Fatalf("wire digest = %+v / %q", attempt.Manifest.WireBody, attempt.Manifest.Digest)
			}
			loaded, err := a.LoadPreparedRequest(attempt.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(req, loaded) {
				t.Fatalf("reconstructed request differs:\nwant=%+v\ngot=%+v", req, loaded)
			}
			wire, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := p.loadPreparedRequest(attempt.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(wire, prepared.Wire) {
				t.Fatalf("wire changed during journal round trip:\nwant=%s\ngot=%s", wire, prepared.Wire)
			}
			if !memory {
				if _, err := os.Stat(filepath.Join(cfg.SessionDir, "journal-session", "blobs", attempt.Manifest.WireBody.Hash)); err != nil {
					t.Fatalf("wire blob missing on disk: %v", err)
				}
			} else if _, err := os.Stat(cfg.SessionDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("memory mode touched disk: stat=%v", err)
			}
			if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 11, CompletionTok: 5, CachedTokens: 2}, nil); err != nil {
				t.Fatal(err)
			}
			if got := a.Usage(); got.InputTokens != 11 || got.OutputTokens != 5 || got.CachedTokens != 2 || got.TurnCount != 1 || math.Abs(got.Cost-0.000021) > 1e-15 {
				t.Fatalf("live usage = %+v", got)
			}
			records, err := p.Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			var finished int
			var absolute *session.UsageChanged
			for _, record := range records {
				switch event := record.Event.(type) {
				case *session.AttemptFinished:
					finished++
				case *session.UsageChanged:
					copy := *event
					absolute = &copy
				}
			}
			if finished != 1 || absolute == nil || absolute.Usage.InputTokens != 11 || absolute.Usage.TotalTokens != 16 {
				t.Fatalf("journal accounting records=%d absolute=%+v", finished, absolute)
			}
		})
	}
}

// cfgValue makes the intended metadata snapshot explicit at call sites while
// keeping the helper independent of pointer ownership.
func cfgValue(cfg config.Config) config.Config { return cfg }

func TestRequestJournalBlobMissingAndTamperedAreExplicit(t *testing.T) {
	a, p, cfg := newJournalAgent(t, false)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider-a", "https://example.test/v1", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(cfg.SessionDir, "journal-session", "blobs", attempt.Manifest.WireBody.Hash)
	if err := os.WriteFile(blobPath, []byte(`{"model":"tampered","stream":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreparedRequest(cfg.SessionDir, "journal-session", attempt.RequestID); !errors.Is(err, ErrPreparedRequestBlobCorrupt) {
		t.Fatalf("tampered load err=%v, want blob corruption", err)
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadPreparedRequest(attempt.RequestID); !errors.Is(err, ErrPreparedRequestBlobMissing) {
		t.Fatalf("missing load err=%v, want missing blob", err)
	}
	if p.Failure() != nil {
		t.Fatalf("read-only load should not poison writer: %v", p.Failure())
	}
}

func TestRequestJournalFailurePoisonsAdmissionButCancellationDoesNot(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.recordPreparedRequest(ctx, "main", "provider-a", "https://example.test/v1", journalRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prepare err=%v", err)
	}
	if got := a.persistenceFailure(); got != nil {
		t.Fatalf("cancelled prepare poisoned persistence: %v", got)
	}
	p.mu.Lock()
	underlying := p.store
	p.store = failingCommitStore{Store: underlying}
	p.mu.Unlock()
	if _, err := a.recordPreparedRequest(context.Background(), "main", "provider-a", "https://example.test/v1", journalRequest()); !errors.Is(err, session.ErrPersistenceFailed) {
		t.Fatalf("failed RequestPrepared err=%v", err)
	}
	if err := a.persistenceFailure(); !errors.Is(err, session.ErrPersistenceFailed) {
		t.Fatalf("failed prepare did not poison runtime: %v", err)
	}
}

func TestRequestJournalAttemptErrorRedactsURLAndConfiguredKey(t *testing.T) {
	a, _, cfg := newJournalAgent(t, true)
	cfg.APIKey = "config-secret"
	cfg.Providers = map[string]config.ProviderConfig{
		"provider-a": {BaseURL: "https://example.test/v1", APIKey: "provider-secret", Models: []string{"journal-model"}},
	}
	ctx := withRequestJournalMetadata(context.Background(), requestJournalMetadata{Config: cfgValue(*cfg), SettingsRevision: 2, Turn: 1, Step: 1, HistoryLen: 3})
	attempt, err := a.recordPreparedRequest(ctx, "main", "provider-a", "https://example.test/v1", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.tokenBaseline.promptTokens = 99
	a.tokenBaseline.historyLen = 77
	a.mu.Unlock()
	networkErr := &url.Error{Op: "Post", URL: "https://user:pass@example.test/v1/chat?api_key=provider-secret&foo=query-secret", Err: errors.New("Authorization: Bearer provider-secret")}
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{}, networkErr); err != nil {
		t.Fatal(err)
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if event, ok := record.Event.(*session.AttemptFinished); ok {
			if strings.Contains(event.Attempt.Error, "provider-secret") || strings.Contains(event.Attempt.Error, "query-secret") || strings.Contains(event.Attempt.Error, "user:pass") {
				t.Fatalf("secret leaked in attempt error: %q", event.Attempt.Error)
			}
			if !strings.Contains(event.Attempt.Error, "example.test/v1/chat") {
				t.Fatalf("safe endpoint context lost: %q", event.Attempt.Error)
			}
			a.mu.Lock()
			baseline := a.tokenBaseline
			a.mu.Unlock()
			if baseline.promptTokens != 99 || baseline.historyLen != 77 {
				t.Fatalf("failed attempt changed token baseline: %+v", baseline)
			}
			return
		}
	}
	t.Fatal("AttemptFinished was not recorded")
}

func TestRequestJournalCapturesStepMetadataBeforeFinish(t *testing.T) {
	a, _, cfg := newJournalAgent(t, true)
	cfg.Providers = map[string]config.ProviderConfig{
		"provider-a": {BaseURL: "https://example.test/v1", APIKey: "frozen-key", Models: []string{"journal-model"}},
	}
	cfg.Pricing = map[string]config.Pricing{"journal-model": {Input: 3, Output: 5}}
	stepConfig := cloneConfig(cfg)
	ctx := withRequestJournalMetadata(context.Background(), requestJournalMetadata{
		Config: stepConfig, SettingsRevision: 8, Turn: 4, Step: 5, HistoryLen: 19,
	})
	attempt, err := a.recordPreparedRequest(ctx, "main", "provider-a", "https://example.test/v1", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.cfg.Pricing["journal-model"] = config.Pricing{Input: 100, Output: 100}
	a.cfg.Providers["provider-a"] = config.ProviderConfig{APIKey: "live-key", Models: []string{"journal-model"}}
	a.history = append(a.history, messages.Message{Content: "future history"})
	a.mu.Unlock()
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 2, CompletionTok: 4}, nil); err != nil {
		t.Fatal(err)
	}
	got := a.Usage()
	if math.Abs(got.Cost-0.000026) > 1e-15 {
		t.Fatalf("finish used live pricing instead of step pricing: %+v", got)
	}
	a.mu.Lock()
	baseline := a.tokenBaseline
	a.mu.Unlock()
	if baseline.promptTokens != 2 || baseline.historyLen != 19 {
		t.Fatalf("finish used live history baseline: %+v", baseline)
	}
}

func TestRequestJournalUsageSubscriberCanSave(t *testing.T) {
	a, _, _ := newJournalAgent(t, false)
	called := false
	var saveErr error
	a.evbus.Subscribe(events.TopicUsageUpdated, func(events.Topic, events.Payload) {
		called = true
		saveErr = a.Save()
	})
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider-a", "https://example.test/v1", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 1, CompletionTok: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("usage subscriber was not called")
	}
	if saveErr != nil {
		t.Fatalf("usage subscriber could not re-enter Save: %v", saveErr)
	}
}

func TestLoadSessionReplaysCreatedParentID(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	child, err := openSessionPersistenceModeWithParent(&cfg, "child-session", time.Unix(10, 0).UTC(), "task", false, "parent-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, "child-session")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ParentID != "parent-session" {
		t.Fatalf("offline parent id = %q, want parent-session", snapshot.ParentID)
	}
	store, err := session.OpenJSONLReadOnly(cfg.SessionDir, "child-session")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records, err := session.ReadAll(store)
	if err != nil {
		t.Fatal(err)
	}
	created, ok := records[0].Event.(*session.SessionCreated)
	if !ok {
		t.Fatalf("first event = %T, want SessionCreated", records[0].Event)
	}
	if created.Source != "task" || created.ParentID != "parent-session" {
		t.Fatalf("created lineage = source %q parent %q", created.Source, created.ParentID)
	}
}

type requestJournalGateProvider struct {
	calls atomic.Int32
}

func (p *requestJournalGateProvider) Name() string { return "request-journal-gate" }

func (p *requestJournalGateProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	p.calls.Add(1)
	return llm.StreamResult{Text: "must-not-succeed", FinishReason: "stop", PromptTokens: 7, CompletionTok: 3}, nil
}

type requestJournalFailEventStore struct {
	session.Store
	event session.EventType
}

func (s requestJournalFailEventStore) Commit(cursor session.Cursor, batch session.Batch) (session.CommitResult, error) {
	for _, event := range batch.Events {
		if event.Type() == s.event {
			return session.CommitResult{}, fmt.Errorf("%w: injected %s", session.ErrPersistenceFailed, s.event)
		}
	}
	return s.Store.Commit(cursor, batch)
}

func newJournalRuntimeAgent(t *testing.T, provider llm.Provider) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
	cfg.Model = "request-journal-gate-model"
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.MaxTurns = 2
	registry := plugin.NewModelRegistry()
	registry.Register(provider)
	registry.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
	return a
}

func submitJournalRuntimeInput(t *testing.T, a *Agent) {
	t.Helper()
	receipt, err := a.Submit(context.Background(), protocol.NewSubmitInput(
		protocol.CommandID("journal-gate-input"), protocol.SessionID(a.SessionID()),
		protocol.InputID("journal-gate-input"), "fail journal", protocol.InputSteer,
	))
	if err != nil || receipt.Rejected() {
		t.Fatalf("journal gate submit: %+v %v", receipt, err)
	}
}

func waitJournalRuntimeIdle(t *testing.T, a *Agent) protocol.SessionView {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		view, err := a.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !view.Busy && view.LastTurn != nil {
			return view
		}
		select {
		case <-deadline.C:
			t.Fatalf("journal gate runtime did not finish: %+v", view.LastTurn)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRequestJournalFailureStopsRealLoop(t *testing.T) {
	for _, kind := range []session.EventType{session.EventTypeRequestPrepared, session.EventTypeAttemptFinished} {
		t.Run(string(kind), func(t *testing.T) {
			provider := &requestJournalGateProvider{}
			a := newJournalRuntimeAgent(t, provider)
			a.mu.Lock()
			a.fallbackClient = provider
			a.cfg.FallbackModel = "fallback-model"
			a.mu.Unlock()
			persistence := a.persistenceHandle()
			persistence.mu.Lock()
			persistence.store = requestJournalFailEventStore{Store: persistence.store, event: kind}
			persistence.mu.Unlock()
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			wantCalls := int32(0)
			if kind == session.EventTypeAttemptFinished {
				wantCalls = 1
			}
			if got := provider.calls.Load(); got != wantCalls {
				t.Fatalf("provider calls = %d, want %d", got, wantCalls)
			}
			if view.LastTurn.Status != protocol.TurnFailed {
				t.Fatalf("journal failure reported non-failed turn: %+v", view.LastTurn)
			}
			if !errors.Is(a.persistenceFailure(), session.ErrPersistenceFailed) {
				t.Fatalf("missing persistent failure: %v", a.persistenceFailure())
			}
			if got := a.Usage(); got.InputTokens != 0 || got.OutputTokens != 0 || got.TurnCount != 0 {
				t.Fatalf("uncommitted usage published: %+v", got)
			}
		})
	}
}
