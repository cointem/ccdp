package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// formalToolFactProvider drives a real Submit/Run loop through two exclusive
// calls. The second response terminates the turn after the projected results
// have been returned to the provider.
type formalToolFactProvider struct {
	calls atomic.Int32
	args  string
}

func (p *formalToolFactProvider) Name() string { return "formal-tool-fact-provider" }

func (p *formalToolFactProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) > 1 {
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}
	firstArgs := p.args
	if firstArgs == "" {
		firstArgs = `{}`
	}
	return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
		{ID: "formal-first", Type: "function", Function: llm.Function{Name: "FormalAuditEffect", Arguments: llm.ArgumentsJSON(firstArgs)}},
		{ID: "formal-second", Type: "function", Function: llm.Function{Name: "FormalAuditEffect", Arguments: llm.ArgumentsJSON(`{}`)}},
	}}, nil
}

type formalAuditEffect struct {
	t     *testing.T
	a     *Agent
	count atomic.Int32
}

func (*formalAuditEffect) Name() string { return "FormalAuditEffect" }

func (*formalAuditEffect) Description() string { return "formal persistence fixture" }

func (*formalAuditEffect) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (tool *formalAuditEffect) Run(*tools.Context) (string, error) {
	count := tool.count.Add(1)
	records, err := tool.a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		return "", err
	}
	starts, finishes := 0, 0
	for _, record := range records {
		switch record.Event.Type() {
		case session.EventTypeToolStarted:
			starts++
		case session.EventTypeToolFinished:
			finishes++
		}
	}
	if starts < int(count) {
		tool.t.Errorf("tool side effect ran before durable ToolStarted: starts=%d calls=%d", starts, count)
	}
	if count == 2 && finishes != 1 {
		tool.t.Errorf("second exclusive effect ran before first ToolFinished: finishes=%d", finishes)
	}
	return "formal effect outcome", nil
}

func TestFormalToolFactsGateRealLoop(t *testing.T) {
	for _, failure := range []session.EventType{"", session.EventTypeToolStarted, session.EventTypeToolFinished} {
		t.Run(string(failure), func(t *testing.T) {
			provider := &formalToolFactProvider{}
			a := newJournalRuntimeAgent(t, provider)
			fixture := &formalAuditEffect{t: t, a: a}
			a.registry.Register(fixture)
			if failure != "" {
				p := a.persistenceHandle()
				p.mu.Lock()
				p.store = requestJournalFailEventStore{Store: p.store, event: failure}
				p.mu.Unlock()
			}
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			want := int32(2)
			if failure == session.EventTypeToolStarted {
				want = 0
			} else if failure == session.EventTypeToolFinished {
				want = 1
			}
			if got := fixture.count.Load(); got != want {
				t.Fatalf("effect calls=%d, want %d", got, want)
			}
			if failure != "" && view.LastTurn.Status != protocol.TurnFailed {
				t.Fatalf("persistence failure reported success: %+v", view.LastTurn)
			}
		})
	}
}

func TestFormalMalformedToolArgumentsHaveNoEffect(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"broken":`, `{} {}`} {
		t.Run(raw, func(t *testing.T) {
			provider := &formalToolFactProvider{args: raw}
			a := newJournalRuntimeAgent(t, provider)
			fixture := &formalAuditEffect{t: t, a: a}
			a.registry.Register(fixture)
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			if got := fixture.count.Load(); got != 0 {
				t.Fatalf("malformed provider call executed %d effects", got)
			}
			if view.LastTurn.Status != protocol.TurnFailed {
				t.Fatalf("malformed call should fail the turn: %+v", view.LastTurn)
			}
		})
	}
}

func formalHookAgent(t *testing.T, provider llm.Provider, hook hooks.HookSpec) *Agent {
	t.Helper()
	cfg := isolatedTestConfig(t, "formal-hook-model")
	cfg.Hooks = hooks.Config{hooks.EventUserPromptSubmit: []hooks.HookSpec{hook}}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
	return a
}

func TestFormalHookFactsGateAndRecordRealHook(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := formalHookAgent(t, provider, hooks.HookSpec{Command: `printf '%s' '{}'`})
	submitJournalRuntimeInput(t, a)
	waitJournalRuntimeIdle(t, a)
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished int
	for _, record := range records {
		switch record.Event.Type() {
		case session.EventTypeHookStarted:
			started++
		case session.EventTypeHookFinished:
			finished++
		}
	}
	if started != 1 || finished != 1 {
		t.Fatalf("hook facts started=%d finished=%d, want one each", started, finished)
	}
}

func TestFormalHookStartFailureDoesNotRunExternalHook(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "hook-must-not-run")
	provider := &formalCompletionProvider{}
	a := formalHookAgent(t, provider, hooks.HookSpec{Command: "touch " + sentinel})
	p := a.persistenceHandle()
	p.mu.Lock()
	p.store = requestJournalFailEventStore{Store: p.store, event: session.EventTypeHookStarted}
	p.mu.Unlock()
	submitJournalRuntimeInput(t, a)
	view := waitJournalRuntimeIdle(t, a)
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook ran after HookStarted failure: %v", err)
	}
	if provider.requestsCount() != 0 {
		t.Fatalf("provider ran after hook admission failure: %d", provider.requestsCount())
	}
	if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("hook admission failure did not fail turn: %+v", view.LastTurn)
	}
}

type formalCompletionProvider struct {
	mu       sync.Mutex
	requests []llm.CompletionRequest
	fail     bool
}

func (p *formalCompletionProvider) Name() string { return "formal-completion-provider" }

func (p *formalCompletionProvider) Stream(_ context.Context, request llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	if p.fail {
		return llm.StreamResult{}, errors.New("controlled compaction provider failure")
	}
	return llm.StreamResult{Text: "fresh response", FinishReason: "stop", PromptTokens: 7, CompletionTok: 3}, nil
}

func (p *formalCompletionProvider) requestsCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func formalCompletionAgent(t *testing.T, provider *formalCompletionProvider) (*Agent, config.Config) {
	t.Helper()
	cfg := isolatedTestConfig(t, "formal-completion-model")
	cfg.MaxTurns = 3
	cfg.KeepAfterCompact = 4
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
	return a, cfg
}

func formalCompletionTurn(t *testing.T, a *Agent, id, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watch, err := a.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	receipt, err := a.Submit(ctx, protocol.NewSubmitInput(protocol.CommandID(id), protocol.SessionID(a.SessionID()), protocol.InputID(id), text, protocol.InputSteer))
	if err != nil || receipt.Rejected() {
		t.Fatalf("submit=%+v err=%v", receipt, err)
	}
	for {
		select {
		case _, ok := <-watch.Updates():
			if !ok {
				t.Fatal("watch closed before turn completion")
			}
			view, snapshotErr := a.Snapshot(ctx)
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			if !view.Busy && view.LastTurn != nil {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestFormalHistoricalImageIsFrozenAcrossTurns(t *testing.T) {
	provider := &formalCompletionProvider{}
	a, cfg := formalCompletionAgent(t, provider)
	imagePath := filepath.Join(cfg.Workspace, "formal.png")
	writeImage := func(c color.RGBA) {
		fixture := image.NewRGBA(image.Rect(0, 0, 1, 1))
		fixture.Set(0, 0, c)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, fixture); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(imagePath, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeImage(color.RGBA{R: 255, A: 255})
	formalCompletionTurn(t, a, "formal-image-first", "look ![formal](formal.png)")
	writeImage(color.RGBA{G: 255, A: 255})
	formalCompletionTurn(t, a, "formal-image-second", "continue without changing the earlier image")

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d, want 2", len(provider.requests))
	}
	findUser := func(request llm.CompletionRequest) []byte {
		for _, message := range request.Messages {
			if message.Role == "user" {
				data, _ := json.Marshal(message.Content)
				return data
			}
		}
		t.Fatal("request has no user message")
		return nil
	}
	first, second := findUser(provider.requests[0]), findUser(provider.requests[1])
	if !bytes.Equal(first, second) {
		t.Fatalf("historical image changed across turns:\n%s\n%s", first, second)
	}
}

func TestFormalMissingFrozenImageBlobStopsProvider(t *testing.T) {
	provider := &formalCompletionProvider{}
	a, cfg := formalCompletionAgent(t, provider)
	imagePath := filepath.Join(cfg.Workspace, "missing-after-freeze.png")
	fixture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	fixture.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	formalCompletionTurn(t, a, "formal-image-missing-first", "capture ![formal](missing-after-freeze.png)")

	var hash string
	for _, message := range a.History() {
		if message.Role == messages.RoleUser && len(message.ImageAttachments) > 0 {
			hash = message.ImageAttachments[0].BlobHash
			break
		}
	}
	if hash == "" {
		t.Fatal("first turn did not persist a frozen image blob")
	}
	p := a.persistenceHandle()
	p.mu.Lock()
	memoryArtifacts := p.memoryArtifacts
	p.mu.Unlock()
	if memoryArtifacts == nil {
		t.Fatal("memory artifact store was not initialized")
	}
	memoryArtifacts.mu.Lock()
	delete(memoryArtifacts.blobs, hash)
	memoryArtifacts.mu.Unlock()

	formalCompletionTurn(t, a, "formal-image-missing-second", "continue after the image blob disappeared")
	if providerRequests := func() int {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return len(provider.requests)
	}(); providerRequests != 1 {
		t.Fatalf("provider requests after missing image=%d, want 1", providerRequests)
	}
	view, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("missing image did not fail the turn: %+v", view.LastTurn)
	}
}

func TestFormalCompactFailureKeepsHistory(t *testing.T) {
	a, _ := formalCompletionAgent(t, &formalCompletionProvider{fail: true})
	for i := 0; i < 5; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "old user", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		call := messages.ToolCall{ID: string(rune('a' + i)), Name: "Read", Arguments: map[string]any{"file_path": "old"}}
		if err := a.appendHistory(messages.AssistantWithTools("", []messages.ToolCall{call})); err != nil {
			t.Fatal(err)
		}
		if err := a.appendHistory(messages.NewToolResult(call, strings.Repeat("important-old-result-", 100), false)); err != nil {
			t.Fatal(err)
		}
	}
	before := a.History()
	a.compactContext(context.Background())
	if got := a.History(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed compaction modified confirmed history:\n got=%+v\nwant=%+v", got, before)
	}
}

func TestFormalSettingsFailureDoesNotPublish(t *testing.T) {
	for _, commandType := range []protocol.CommandType{protocol.CommandSetExecutionMode, protocol.CommandSetPermissionPolicy, protocol.CommandSetSandboxPolicy} {
		t.Run(string(commandType), func(t *testing.T) {
			a := newJournalRuntimeAgent(t, &formalCompletionProvider{})
			before, err := a.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p := a.persistenceHandle()
			p.mu.Lock()
			p.store = requestJournalFailEventStore{Store: p.store, event: session.EventTypeSettingsChanged}
			p.mu.Unlock()
			command := protocol.Command{ID: protocol.CommandID("formal-settings-failure"), SessionID: protocol.SessionID(a.SessionID()), Type: commandType}
			switch commandType {
			case protocol.CommandSetExecutionMode:
				command.ExecutionMode = &protocol.SetExecutionMode{Mode: protocol.ExecutionModePlan}
			case protocol.CommandSetPermissionPolicy:
				command.PermissionPolicy = &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: "default", AlwaysDeny: []string{"FormalAuditEffect"}}}
			case protocol.CommandSetSandboxPolicy:
				command.SandboxPolicy = &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{NetworkAccess: true}}
			}
			receipt, submitErr := a.Submit(context.Background(), command)
			if submitErr != nil {
				t.Fatal(submitErr)
			}
			if !receipt.Rejected() {
				t.Fatalf("failed settings commit acknowledged success: %+v", receipt)
			}
			after, err := a.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Settings, after.Settings) || before.Workflow != after.Workflow {
				t.Fatalf("uncommitted settings became live:\n before=%+v\n after=%+v", before.Settings, after.Settings)
			}
		})
	}
}

func TestFormalOversizedInstructionsAreRetryableInputFailure(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	cfg := a.configSnapshot()
	path := filepath.Join(cfg.Workspace, workspace.InstructionFile)
	if err := os.WriteFile(path, []byte(strings.Repeat("mandatory instruction ", 18000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.LoadInstructionsChecked(cfg.Workspace); err == nil {
		t.Fatal("checked instruction reader accepted an oversized AGENTS.md")
	}
	formalCompletionTurn(t, a, "formal-oversized-input", "input with oversized instructions")
	if provider.requestsCount() != 0 {
		t.Fatalf("provider called with rejected instructions: %d", provider.requestsCount())
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("input failure poisoned persistence: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	formalCompletionTurn(t, a, "formal-oversized-input-retry", "retry after fixing instructions")
	if provider.requestsCount() != 1 {
		t.Fatalf("provider requests after retry=%d, want 1", provider.requestsCount())
	}
}

func waitFormalCommandReceipt(t *testing.T, a *Agent, id protocol.CommandID) protocol.Receipt {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		receipt, ok := a.seenReceipts[id]
		a.mu.Unlock()
		if ok && receipt.Status != protocol.ReceiptScheduled {
			return receipt
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("command did not reach a terminal receipt")
	return protocol.Receipt{}
}

// TestFormalCommandFactsPreventResumeRetry exercises the real command loop:
// admission is durable before the export worker starts, a terminal completion
// carries the bounded output artifact, and a crash window (the completion
// commit is rejected) is recovered as unknown rather than re-running the
// external write.  A command ID with a different body is rejected in either
// case because only its durable full-body digest is trusted on resume.
func TestFormalCommandFactsPreventResumeRetry(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "unknown"}[unknown], func(t *testing.T) {
			a := newJournalRuntimeAgent(t, &formalCompletionProvider{})
			cfg := a.configSnapshot()
			path := filepath.Join(cfg.Workspace, "formal-export.md")
			cmd := protocol.Command{ID: "formal-export-once", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandExport, Export: &protocol.ExportCommand{Path: path}}
			if unknown {
				p := a.persistenceHandle()
				p.mu.Lock()
				p.store = requestJournalFailEventStore{Store: p.store, event: session.EventTypeCommandCompleted}
				p.mu.Unlock()
			}
			admitted, err := a.Submit(context.Background(), cmd)
			if err != nil || admitted.Rejected() || admitted.Status != protocol.ReceiptScheduled {
				t.Fatalf("command admission = %+v, err=%v", admitted, err)
			}
			terminal := waitFormalCommandReceipt(t, a, cmd.ID)
			if terminal.Rejected() != unknown {
				t.Fatalf("terminal receipt = %+v, unknown=%v", terminal, unknown)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("export side effect: %v", err)
			}
			if !unknown {
				records, readErr := a.persistenceHandle().Read(session.Beginning)
				if readErr != nil {
					t.Fatal(readErr)
				}
				foundOutput := false
				for _, record := range records {
					if event, ok := record.Event.(*session.CommandCompleted); ok && event.CommandID == string(cmd.ID) {
						foundOutput = event.Output != nil
					}
				}
				if !foundOutput {
					t.Fatal("completed command has no durable output artifact")
				}
			}

			sessionID := a.SessionID()
			_ = a.CloseContext(context.Background())
			marker := "must not be overwritten by a resumed retry"
			if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := LoadSession(cfg.SessionDir, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(&cfg, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			retry, err := resumed.Submit(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if retry.Rejected() != unknown {
				t.Fatalf("resumed retry = %+v, unknown=%v", retry, unknown)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != marker {
				t.Fatalf("same command ID repeated export after resume: %q", data)
			}
			different := cmd
			different.Export = &protocol.ExportCommand{Path: filepath.Join(cfg.Workspace, "formal-export-different.md")}
			conflict, err := resumed.Submit(context.Background(), different)
			if err != nil {
				t.Fatal(err)
			}
			if !conflict.Rejected() {
				t.Fatalf("different command body accepted after resume: %+v", conflict)
			}
			if _, err := os.Stat(different.Export.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("different-body command had side effects: %v", err)
			}
		})
	}
}
