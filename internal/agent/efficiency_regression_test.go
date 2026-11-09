package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func TestWaitDoesNotSerializeIndependentRead(t *testing.T) {
	for _, code := range []bool{false, true} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var a *Agent
			var count int
			var id string
			released := make(chan error, 1)
			provider := &childRuntimeTestProvider{name: "wait-concurrency", stream: func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
				count++
				call := func(name string, args map[string]any) llm.ToolCall {
					b, _ := json.Marshal(args)
					return llm.ToolCall{ID: fmt.Sprintf("call-%d-%s", count, name), Type: "function", Function: llm.Function{Name: name, Arguments: llm.ArgumentsJSON(b)}}
				}
				if count == 1 {
					return llm.StreamResult{ToolCalls: []llm.ToolCall{call("Bash", map[string]any{"command": "cat", "stdin": "pipe"})}, FinishReason: "tool_calls"}, nil
				}
				if count == 2 {
					for _, m := range a.History() {
						if m.Role == messages.RoleTool {
							if _, value, ok := strings.Cut(m.Content, "process_id="); ok {
								id = strings.Fields(value)[0]
							}
						}
					}
					if id == "" {
						return llm.StreamResult{}, fmt.Errorf("missing process")
					}
					go func() {
						deadline := time.Now().Add(10 * time.Second)
						for time.Now().Before(deadline) {
							rows, err := a.persistenceHandle().Read(session.Beginning)
							if err != nil {
								released <- err
								return
							}
							waitStarted, readStarted := false, false
							for _, r := range rows {
								switch e := r.Event.(type) {
								case *session.ToolStarted:
									waitStarted = waitStarted || e.Call.ToolID == "Process"
									readStarted = readStarted || e.Call.ToolID == "Read"
								case session.ToolStarted:
									waitStarted = waitStarted || e.Call.ToolID == "Process"
									readStarted = readStarted || e.Call.ToolID == "Read"
								}
							}
							if waitStarted && readStarted {
								tc := a.services.resources.Context(ctx, a.configSnapshot().Workspace, a.currentSandbox())
								_, err := tools.NewProcessTool().Execute(tc, map[string]any{"id": id, "action": "write", "eof": true})
								released <- err
								return
							}
							time.Sleep(time.Millisecond)
						}
						tc := a.services.resources.Context(ctx, a.configSnapshot().Workspace, a.currentSandbox())
						_, _ = tools.NewProcessTool().Execute(tc, map[string]any{"id": id, "action": "write", "eof": true})
						released <- fmt.Errorf("Read did not start alongside Process.wait")
					}()
					if code {
						return llm.StreamResult{ToolCalls: []llm.ToolCall{call("RunCode", map[string]any{"code": fmt.Sprintf(`await Promise.all([tools.Process({id:%q,action:"wait"}),tools.Read({file_path:"input.txt"})]);text("done");`, id)})}, FinishReason: "tool_calls"}, nil
					}
					return llm.StreamResult{ToolCalls: []llm.ToolCall{call("Process", map[string]any{"id": id, "action": "wait"}), call("Read", map[string]any{"file_path": "input.txt"})}, FinishReason: "tool_calls"}, nil
				}
				return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
			}}
			a = newJournalRuntimeAgent(t, provider)
			a.services.cfg.MaxTurns = 4
			if err := os.WriteFile(filepath.Join(a.configSnapshot().Workspace, "input.txt"), []byte("data\n"), 0600); err != nil {
				t.Fatal(err)
			}
			events := watchTestEvents(t, a)
			submitJournalRuntimeInput(t, a)
			drainUntil(t, events, 20*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
			select {
			case err := <-released:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("release missing")
			}
			rows, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			readIndex, finishIndex := -1, -1
			for i, r := range rows {
				switch e := r.Event.(type) {
				case *session.ToolStarted:
					if e.Call.ToolID == "Read" {
						readIndex = i
					}
				case session.ToolStarted:
					if e.Call.ToolID == "Read" {
						readIndex = i
					}
				case *session.ProcessFinished:
					if e.State.ID == id {
						finishIndex = i
					}
				case session.ProcessFinished:
					if e.State.ID == id {
						finishIndex = i
					}
				}
			}
			t.Logf("code=%t read_start_index=%d process_finish_index=%d read_started_before_wait_finished=%t", code, readIndex, finishIndex, readIndex >= 0 && readIndex < finishIndex)
			if readIndex < 0 || finishIndex < 0 {
				t.Fatal("missing lifecycle evidence")
			}
			if readIndex > finishIndex {
				t.Errorf("independent Read blocked behind Process.wait")
			}
		})
	}
}

func TestRepeatedDenialStopsAtThreeModelRequests(t *testing.T) {
	for _, code := range []bool{false, true} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			outside := filepath.Join(t.TempDir(), "denied.txt")
			args := map[string]any{"file_path": outside, "content": "never write"}
			encoded, _ := json.Marshal(args)
			toolName := "Write"
			if code {
				toolName = "RunCode"
				encoded, _ = json.Marshal(map[string]any{"code": fmt.Sprintf("try { await tools.Write(%s); } catch (e) { text(e.status); }", encoded)})
			}
			provider := &childRuntimeTestProvider{name: "denial-loop", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				n := calls.Add(1)
				return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("denied-%d", n), Type: "function", Function: llm.Function{Name: toolName, Arguments: llm.ArgumentsJSON(encoded)}}}, FinishReason: "tool_calls"}, nil
			}}
			a := newJournalRuntimeAgent(t, provider)
			a.services.cfg.MaxTurns = 5
			a.services.cfg.PermissionMode = string(permissions.ModeAuto)
			a.services.perms.SetMode(permissions.ModeAuto)
			call := messages.ToolCall{Name: "Write", Arguments: args}
			_, missing, _, err := a.toolCapabilityPolicy(call, tools.NewWriteTool())
			if err != nil || len(missing) == 0 {
				t.Fatalf("missing capabilities=%+v err=%v", missing, err)
			}
			key := a.resourceDecisionKey(call, missing, true)
			a.mu.Lock()
			a.cacheResourceDecisionLocked(key, approvalAnswer{})
			a.mu.Unlock()
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			if calls.Load() != 3 || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnBlocked {
				t.Fatalf("same cached denial: requests=%d turn=%+v", calls.Load(), view.LastTurn)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatal("denied operation executed", err)
			}
		})
	}
}

func TestModelStepLimitIsNotTaskSuccess(t *testing.T) {
	var calls atomic.Int32
	p := &childRuntimeTestProvider{name: "step-budget", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: fmt.Sprint(n), Type: "function", Function: llm.Function{Name: "GetGoal", Arguments: llm.ArgumentsJSON(`{}`)}}}, FinishReason: "tool_calls"}, nil
	}}
	a := newJournalRuntimeAgent(t, p)
	a.services.cfg.MaxTurns = 2
	submitJournalRuntimeInput(t, a)
	v := waitJournalRuntimeIdle(t, a)
	t.Logf("model_requests=%d final_response=false outcome=%+v", calls.Load(), v.LastTurn)
	if calls.Load() != 2 || v.LastTurn == nil || v.LastTurn.Status != protocol.TurnLimited {
		t.Fatal("step limit was not preserved", v.LastTurn)
	}
	rows, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if end, ok := row.Event.(*session.TurnFinished); ok {
			found = end.Outcome == "limit_reached"
		}
	}
	if !found {
		t.Fatal("durable outcome lost step limit")
	}
}

func TestDevelopmentCacheSurvivesOwnerRestart(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = t.TempDir()
	cfg.EnableProjectMemory = false
	cfg.NoSessionPersistence = true
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "go.mod"), []byte("module audit\n\ngo 1.25.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "smoke_test.go"), []byte("package audit\nimport \"testing\"\nfunc TestSmoke(t *testing.T) {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(a *Agent, label string) string {
		tc := a.services.resources.Context(context.Background(), cfg.Workspace, a.currentSandbox())
		start := time.Now()
		r, err := tools.NewBashTool().Execute(tc, map[string]any{"command": "printf 'CACHE=%s\\n' \"$GOCACHE\"; go test -count=1 ./...", "timeout_ms": 120000})
		var p protocol.ProcessOutput
		decode := json.Unmarshal(r.Data, &p)
		t.Logf("%s elapsed=%s result=%s err=%v decode=%v", label, time.Since(start), r.Text, err, decode)
		if err != nil || decode != nil || p.ExitCode == nil || *p.ExitCode != 0 {
			t.Fatal("build probe failed")
		}
		line, _, _ := strings.Cut(p.Output, "\n")
		return strings.TrimPrefix(line, "CACHE=")
	}
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cache := run(a, "owner1-cold")
	if run(a, "owner1-warm") != cache {
		t.Fatal("cache changed within owner")
	}
	a.Close()
	second, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if next := run(second, "owner2-reused"); next != cache {
		t.Fatalf("cache lost across executor restart: %s != %s", next, cache)
	}
}

func TestDefaultPromptBudget(t *testing.T) {
	for _, code := range []bool{false, true} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			cfg := config.Default()
			cfg.Workspace = t.TempDir()
			cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
			cfg.EnableProjectMemory = false
			cfg.NoSessionPersistence = true
			cfg.CodeModeEnabled = code
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			step, err := a.beginStepChecked()
			if err != nil {
				t.Fatal(err)
			}
			defer a.releaseStepContext(step)
			req, err := a.buildRequestForStepChecked(step)
			if err != nil {
				t.Fatal(err)
			}
			schemas, _ := json.Marshal(req.Tools)
			sys := joinSystemBlocks(req.SystemBlocks)
			runtimeTokens := 0
			for _, message := range step.history {
				if strings.HasPrefix(message.InternalSource, "runtime_") {
					runtimeTokens += llm.EstimateTokens(message.Content)
				}
			}
			if cost := estimateToolsTokens(req.Tools) + llm.EstimateTokens(sys) + runtimeTokens; cost > 8000 {
				t.Fatalf("default prompt grew beyond 8000 estimated tokens: %d", cost)
			}
			t.Logf("initial_runtime_est_tokens=%d total_input_budget=%+v", runtimeTokens, llm.EstimateRequestBudget(req, 0))
			extra := 0
			type size struct {
				Name  string
				Bytes int
			}
			var sizes []size
			for _, d := range req.Tools {
				b, _ := json.Marshal(d)
				sizes = append(sizes, size{d.Function.Name, len(b)})
				if i := strings.Index(d.Function.Description, "RunCode result.data:"); i >= 0 {
					extra += len(d.Function.Description) - i
				}
			}
			sort.Slice(sizes, func(i, j int) bool { return sizes[i].Bytes > sizes[j].Bytes })
			t.Logf("tools=%d schema_json_bytes=%d schema_est_tokens=%d system_bytes=%d system_est_tokens=%d inline_output_schema_bytes=%d largest=%+v", len(req.Tools), len(schemas), estimateToolsTokens(req.Tools), len(sys), llm.EstimateTokens(sys), extra, sizes[:min(8, len(sizes))])
		})
	}
}

func BenchmarkEmptyPendingProcessProjection(b *testing.B) {
	for _, count := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			a := &Agent{}
			cfg := config.Default()
			a.services.cfg = &cfg
			for i := 0; i < count; i++ {
				a.conversation.durable.History = append(a.conversation.durable.History, messages.Message{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: fmt.Sprint(i), Name: "Read", Arguments: map[string]any{"file_path": "source.go"}}}})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := a.projectPendingProcessesOwned(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Mock providers route on task messages, not append-only runtime state.
func isTaskMessage(message llm.ChatMessage) bool {
	text, _ := message.Content.(string)
	return message.Role == "user" && !strings.HasPrefix(text, "<runtime_context ")
}
