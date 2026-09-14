package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

// childRuntimeTestProvider is intentionally a generic llm.Provider. These
// tests exercise the same provider registry/binding used by production child
// construction; no concrete llm.Client assertion is allowed here.
type childRuntimeTestProvider struct {
	name   string
	stream func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error)
}

func (p *childRuntimeTestProvider) Name() string { return p.name }

func (p *childRuntimeTestProvider) Stream(ctx context.Context, req llm.CompletionRequest, delta func(string)) (llm.StreamResult, error) {
	return p.stream(ctx, req, delta)
}

func newChildRuntimeTestAgent(t *testing.T, p llm.Provider) (*Agent, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
	cfg.Model = "child-test-model"
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.MaxTurns = 4
	registry := plugin.NewModelRegistry()
	registry.Register(p)
	registry.Route(cfg.Model, p.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, cfg
}

func submitChildRuntimeTestInput(t *testing.T, a *Agent, id, input string) {
	t.Helper()
	cmd := protocol.NewSubmitInput(protocol.CommandID(id), protocol.SessionID(a.SessionID()), protocol.InputID(id), input, protocol.InputSteer)
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil || receipt.Rejected() {
		t.Fatalf("submit %s: %+v %v", id, receipt, err)
	}
}

func waitChildRuntimeTestIdle(t *testing.T, a *Agent) protocol.SessionView {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
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
			t.Fatalf("runtime did not finish: %+v", view.LastTurn)
		case <-tick.C:
		}
	}
}

func TestTaskChildUsesRealLoopAndAggregatesUsageOnce(t *testing.T) {
	var mu sync.Mutex
	var requests []llm.CompletionRequest
	p := &childRuntimeTestProvider{name: "child-test-provider"}
	p.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		result := llm.StreamResult{PromptTokens: 7, CompletionTok: 3, FinishReason: "stop"}
		lastUser := ""
		var toolResult string
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser, _ = message.Content.(string)
			}
			if message.Role == "tool" {
				toolResult, _ = message.Content.(string)
			}
		}
		switch {
		case lastUser == "independent-child":
			result.Text = "child-answer"
		case toolResult != "":
			if !strings.Contains(toolResult, "child-answer") {
				return result, fmt.Errorf("child result lost: %s", toolResult)
			}
			result.Text = "parent-answer"
		default:
			result.FinishReason = "tool_calls"
			result.ToolCalls = []llm.ToolCall{{ID: "child-call", Type: "function", Function: llm.Function{
				Name: "Task", Arguments: llm.ArgumentsJSON(`{"description":"independent-child"}`),
			}}}
		}
		return result, nil
	}
	a, cfg := newChildRuntimeTestAgent(t, p)
	submitChildRuntimeTestInput(t, a, "delegate", "independent-parent")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status == protocol.TurnFailed {
		t.Fatalf("parent failed: %+v", view.LastTurn)
	}
	if len(view.History) == 0 || view.History[len(view.History)-1].Content != "parent-answer" {
		t.Fatalf("authoritative final missing: %+v", view.History)
	}
	if got := a.Usage(); got.InputTokens != 21 || got.OutputTokens != 9 || got.TurnCount != 3 {
		t.Fatalf("live parent usage: %+v", got)
	}
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadSession(cfg.SessionDir, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Usage != a.Usage() {
		t.Fatalf("replay/live divergence: %+v vs %+v", saved.Usage, a.Usage())
	}
	sessions, err := ListSessions(cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want independent parent + child, got %d", len(sessions))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected 3 registered-provider calls, got %d", len(requests))
	}
}

func TestTaskChildCancellationDoesNotPoisonNextParentTurn(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	p := &childRuntimeTestProvider{name: "cancel-child-test-provider", stream: func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		last := ""
		for _, message := range req.Messages {
			if message.Role == "user" {
				last, _ = message.Content.(string)
			}
		}
		if last == "first" {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{Text: "continued", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	submitChildRuntimeTestInput(t, a, "first-input", "first")
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("provider did not start")
	}
	receipt, err := a.Submit(context.Background(), protocol.Command{ID: "interrupt", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandInterrupt})
	if err != nil || receipt.Rejected() {
		t.Fatalf("interrupt: %+v %v", receipt, err)
	}
	waitChildRuntimeTestIdle(t, a)
	submitChildRuntimeTestInput(t, a, "second-input", "second")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status == protocol.TurnFailed {
		t.Fatalf("cancel poisoned later turn: %+v", view.LastTurn)
	}
	if len(view.History) == 0 || view.History[len(view.History)-1].Content != "continued" {
		t.Fatalf("second result missing: %+v", view.History)
	}
}

func TestTaskChildInterruptJoinsAndAggregatesPartialUsage(t *testing.T) {
	childStarted := make(chan struct{})
	var childStartOnce sync.Once
	p := &childRuntimeTestProvider{name: "interrupt-child-test-provider"}
	p.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		lastUser := ""
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser, _ = message.Content.(string)
			}
		}
		switch lastUser {
		case "cancel-parent":
			return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
				ID: "cancel-child-call", Type: "function", Function: llm.Function{
					Name: "Task", Arguments: llm.ArgumentsJSON(`{"description":"cancel-child"}`),
				},
			}}}, nil
		case "cancel-child":
			childStartOnce.Do(func() { close(childStarted) })
			<-ctx.Done()
			// The request journal must retain usage even when the provider exits
			// because the parent turn was interrupted.
			return llm.StreamResult{PromptTokens: 5, CompletionTok: 2, FinishReason: "stop"}, ctx.Err()
		case "continue-parent":
			return llm.StreamResult{Text: "after-child-interrupt", FinishReason: "stop", PromptTokens: 3, CompletionTok: 1}, nil
		default:
			return llm.StreamResult{Text: "unexpected-input", FinishReason: "stop"}, nil
		}
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	submitChildRuntimeTestInput(t, a, "cancel-parent-input", "cancel-parent")
	select {
	case <-childStarted:
	case <-time.After(4 * time.Second):
		t.Fatal("child provider did not start")
	}
	receipt, err := a.Submit(context.Background(), protocol.Command{
		ID: "interrupt-running-child", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandInterrupt,
	})
	if err != nil || receipt.Rejected() {
		t.Fatalf("interrupt: %+v %v", receipt, err)
	}
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status != protocol.TurnCancelled {
		t.Fatalf("parent turn status after child interrupt=%s, want cancelled", view.LastTurn.Status)
	}
	partial := a.Usage()
	if partial.InputTokens != 5 || partial.OutputTokens != 2 || partial.TurnCount != 2 {
		t.Fatalf("partial child usage was not joined: %+v", partial)
	}

	submitChildRuntimeTestInput(t, a, "continue-parent-input", "continue-parent")
	view = waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status == protocol.TurnFailed || len(view.History) == 0 || view.History[len(view.History)-1].Content != "after-child-interrupt" {
		t.Fatalf("next parent turn did not continue: status=%s history=%+v", view.LastTurn.Status, view.History)
	}
	if got := a.Usage(); got.InputTokens != 8 || got.OutputTokens != 3 || got.TurnCount != 3 {
		t.Fatalf("usage after continuation=%+v, want input=8 output=3 turns=3", got)
	}
}

func TestTaskBatchSharesBoundedChildSlots(t *testing.T) {
	var active int32
	var maxActive int32
	p := &childRuntimeTestProvider{name: "slots-child-test-provider", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
				break
			}
		}
		select {
		case <-time.After(40 * time.Millisecond):
		case <-ctx.Done():
			atomic.AddInt32(&active, -1)
			return llm.StreamResult{}, ctx.Err()
		}
		atomic.AddInt32(&active, -1)
		return llm.StreamResult{Text: "slot-ok", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.mu.Lock()
	// The provider-owned constructor normally sizes this once from the frozen
	// effective config. Install the same bounded parent slot set here without
	// mutating a live config after construction.
	a.childSlots = newChildSlots(2)
	a.mu.Unlock()
	tasks := make([]tools.SubagentTask, 4)
	for i := range tasks {
		tasks[i].Description = fmt.Sprintf("slot-%d", i)
	}
	results, err := a.runSubagents(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(tasks) {
		t.Fatalf("results=%d, want=%d", len(results), len(tasks))
	}
	for i, result := range results {
		if result.Index != i || result.Error != "" || result.Output != "slot-ok" {
			t.Fatalf("result[%d]=%+v", i, result)
		}
	}
	if got := atomic.LoadInt32(&maxActive); got > 2 {
		t.Fatalf("child provider concurrency=%d, want <=2", got)
	}
}

func TestTaskBatchesShareParentChildSlots(t *testing.T) {
	var active int32
	var maxActive int32
	p := &childRuntimeTestProvider{name: "shared-slots-child-test-provider"}
	p.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
				break
			}
		}
		defer atomic.AddInt32(&active, -1)
		select {
		case <-time.After(40 * time.Millisecond):
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		}
		lastUser := ""
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser, _ = message.Content.(string)
			}
		}
		return llm.StreamResult{Text: lastUser, FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.mu.Lock()
	a.childSlots = newChildSlots(2)
	a.mu.Unlock()

	const batchCount = 2
	const tasksPerBatch = 3
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([][]tools.SubagentResult, batchCount)
	errs := make([]error, batchCount)
	for batch := 0; batch < batchCount; batch++ {
		batch := batch
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tasks := make([]tools.SubagentTask, tasksPerBatch)
			for i := range tasks {
				tasks[i].Description = fmt.Sprintf("shared-batch-%d-task-%d", batch, i)
			}
			results[batch], errs[batch] = a.runSubagents(tasks)
		}()
	}
	close(start)
	wg.Wait()
	for batch := range results {
		if errs[batch] != nil {
			t.Fatalf("batch %d: %v", batch, errs[batch])
		}
		if len(results[batch]) != tasksPerBatch {
			t.Fatalf("batch %d results=%d, want=%d", batch, len(results[batch]), tasksPerBatch)
		}
		for i, result := range results[batch] {
			want := fmt.Sprintf("shared-batch-%d-task-%d", batch, i)
			if result.Index != i || result.Error != "" || result.Output != want {
				t.Fatalf("batch %d result[%d]=%+v, want ordered output %q", batch, i, result, want)
			}
		}
	}
	if got := atomic.LoadInt32(&maxActive); got > 2 {
		t.Fatalf("shared child provider concurrency=%d, want <=2", got)
	}
}

func TestTaskChildHooksApplyToSingleAndBatch(t *testing.T) {
	p := &childRuntimeTestProvider{name: "hooks-child-test-provider", stream: func(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "hooked", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	hookFile := filepath.Join(t.TempDir(), "task-hooks.log")
	a.hooks = hooks.NewManager(hooks.Config{
		hooks.EventSubagentStart: {{Command: `printf 'start\n' >> "$CCDP_TEST_HOOK_FILE"`}},
		hooks.EventSubagentStop:  {{Command: `printf 'stop\n' >> "$CCDP_TEST_HOOK_FILE"`}},
	}, hooks.Options{SessionID: a.SessionID(), Workspace: a.cfg.Workspace, Env: []string{"CCDP_TEST_HOOK_FILE=" + hookFile}})
	if _, err := a.runSubagent("single-hook", ""); err != nil {
		t.Fatal(err)
	}
	tasks := []tools.SubagentTask{{Description: "batch-hook-0"}, {Description: "batch-hook-1"}, {Description: "batch-hook-2"}}
	results, err := a.runSubagents(tasks)
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if result.Error != "" || result.Output != "hooked" {
			t.Fatalf("batch result[%d]=%+v", i, result)
		}
	}
	// Guardians have a separate read-only path and must not trigger Task
	// lifecycle hooks.
	if _, err := a.runGuardianChild("guardian-hook"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "start\n"); got != 4 {
		t.Fatalf("SubagentStart count=%d, want 4", got)
	}
	if got := strings.Count(string(data), "stop\n"); got != 4 {
		t.Fatalf("SubagentStop count=%d, want 4", got)
	}
}

func TestTaskChildInheritsFrozenGoPreToolDecision(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "child-go-hook-must-not-run")
	p := &childRuntimeTestProvider{name: "child-go-hook-provider"}
	var calls atomic.Int32
	p.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		for _, message := range req.Messages {
			if message.Role == "tool" {
				return llm.StreamResult{Text: "child completed with the inherited denial", FinishReason: "stop"}, nil
			}
		}
		return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: "child-write", Type: "function", Function: llm.Function{
				Name: "Write", Arguments: llm.ArgumentsJSON(fmt.Sprintf(`{"file_path":%q,"content":"must not write"}`, sentinel)),
			},
		}}}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.gohooks.AddPreTool(func(name string, _ map[string]any) (plugin.ToolDecision, string) {
		if name == "Write" {
			return plugin.DecisionDeny, "parent policy denies child writes"
		}
		return plugin.DecisionNone, ""
	})
	out, err := a.runSubagent("inherit-parent-policy", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "inherited denial") {
		t.Fatalf("child output lost inherited denial: %q", out)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child bypassed inherited Go pre-tool denial: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("provider calls=%d, want child tool call plus final response", calls.Load())
	}
}

func TestTaskChildUsesStepProviderAndConfigSnapshot(t *testing.T) {
	var parent *Agent
	var replaced int32
	var streamReplaced int32
	var mutate sync.Once
	preStepReplacement := &childRuntimeTestProvider{name: "pre-step-replacement-provider", stream: func(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		atomic.AddInt32(&replaced, 1)
		return llm.StreamResult{Text: "wrong-provider-child", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}}
	streamReplacement := &childRuntimeTestProvider{name: "stream-replacement-provider", stream: func(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		atomic.AddInt32(&streamReplaced, 1)
		return llm.StreamResult{Text: "wrong-provider-child", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
	}}
	original := &childRuntimeTestProvider{name: "original-step-provider"}
	original.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		lastUser := ""
		toolResult := ""
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser, _ = message.Content.(string)
			}
			if message.Role == "tool" {
				toolResult, _ = message.Content.(string)
			}
		}
		if toolResult != "" {
			if !strings.Contains(toolResult, "original-child") {
				return llm.StreamResult{}, fmt.Errorf("child used replaced provider: %s", toolResult)
			}
			return llm.StreamResult{Text: "parent-step-final", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
		}
		if lastUser == "step-child" {
			return llm.StreamResult{Text: "original-child", FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
		}
		mutate.Do(func() {
			if parent == nil {
				return
			}
			parent.mu.Lock()
			// Replace both the live route and effective endpoint while the
			// original parent request is still streaming. The active binding and
			// childStep must remain authoritative for the already-admitted Task.
			parent.cfg.Model = "replacement-step-model"
			parent.cfg.BaseURL = "http://replacement.invalid"
			parent.models.Register(streamReplacement)
			parent.models.Route("child-test-model", streamReplacement.Name())
			parent.mu.Unlock()
		})
		return llm.StreamResult{FinishReason: "tool_calls", PromptTokens: 1, CompletionTok: 1,
			ToolCalls: []llm.ToolCall{{ID: "step-task", Type: "function", Function: llm.Function{
				Name: "Task", Arguments: llm.ArgumentsJSON(`{"description":"step-child"}`),
			}}}}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, original)
	parent = a
	// Simulate a registry replacement that landed just before beginStep. The
	// active binding still points at original, but the step's frozen registry
	// therefore starts out pointing at preStepReplacement. pinChildProvider
	// must reconcile that mismatch to the binding captured by the step.
	a.models.Register(preStepReplacement)
	a.models.Route("child-test-model", preStepReplacement.Name())
	submitChildRuntimeTestInput(t, a, "step-input", "step-parent")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status == protocol.TurnFailed {
		t.Fatalf("parent failed: %+v", view.LastTurn)
	}
	if len(view.History) == 0 || view.History[len(view.History)-1].Content != "parent-step-final" {
		t.Fatalf("step result missing: %+v", view.History)
	}
	if got := atomic.LoadInt32(&replaced); got != 0 {
		t.Fatalf("child used a pre-step replacement provider %d time(s)", got)
	}
	if got := atomic.LoadInt32(&streamReplaced); got != 0 {
		t.Fatalf("child used a streaming replacement provider %d time(s)", got)
	}
}

func TestPreparedChildRequestRetainsFrozenWire(t *testing.T) {
	var seen []byte
	p := &childRuntimeTestProvider{name: "wire-child-test-provider", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		var err error
		seen, err = json.Marshal(req)
		return llm.StreamResult{}, err
	}}
	type customPart struct {
		Type    string      `json:"type"`
		Payload string      `json:"payload"`
		Count   json.Number `json:"count"`
	}
	part := &customPart{Type: "custom", Payload: "frozen", Count: json.Number("9007199254740993")}
	req := llm.CompletionRequest{Model: "wire", Messages: []llm.ChatMessage{{Role: "user", Content: []any{part}}}}
	call, err := llm.NewPreparedCall(p, p.Name(), "", req)
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	manifest := call.Manifest()
	part.Payload = "mutated"
	if _, err = call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, manifest.RequestJSON) {
		t.Fatalf("wire changed after preparation:\nmanifest=%s\nsent=%s", manifest.RequestJSON, seen)
	}
	manifest.RequestJSON[0] = 'x'
	if call.Manifest().RequestJSON[0] != '{' {
		t.Fatal("manifest bytes aliased")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	req.Messages[0].Content = cycle
	if _, err = llm.NewPreparedCall(p, p.Name(), "", req); err == nil {
		t.Fatal("cyclic input must return error")
	}
}

func TestChildToolGateHardPolicies(t *testing.T) {
	guardian := &Agent{childState: &childRuntimeState{
		purpose:        childPurposeGuardian,
		nonInteractive: true,
		perms:          permissions.NewManager(permissions.ModeBypass, permissions.Policy{}),
	}}
	for _, name := range []string{"Write", "Edit", "Bash", "WebSearch", "Task", "EnterPlanMode", "ExitPlanMode", "unknown"} {
		if denied, reason := ChildToolGate(guardian, messages.ToolCall{Name: name}); !denied || reason == "" {
			t.Fatalf("guardian tool %q escaped hard gate: denied=%v reason=%q", name, denied, reason)
		}
	}
	if denied, reason := ChildToolGate(guardian, messages.ToolCall{Name: "Read"}); denied {
		t.Fatalf("guardian read unexpectedly denied: %q", reason)
	}

	task := &Agent{childState: &childRuntimeState{
		purpose:        childPurposeTask,
		nonInteractive: true,
		perms:          permissions.NewManager(permissions.ModeDefault, permissions.Policy{}),
	}}
	if denied, reason := ChildToolGate(task, messages.ToolCall{Name: "Task"}); !denied || reason == "" {
		t.Fatalf("nested Task escaped hard gate: denied=%v reason=%q", denied, reason)
	}
	if denied, reason := ChildToolGate(task, messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": "x"}}); !denied || !strings.Contains(reason, "non-interactive") {
		t.Fatalf("approval Ask escaped non-interactive gate: denied=%v reason=%q", denied, reason)
	}
}
