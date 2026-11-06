package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func TestToolOutputBudgetPreservesSessionArtifacts(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk", true: "memory"}[memory], func(t *testing.T) {
			cfg := isolatedTestConfig(t, "output-budget-model")
			cfg.NoSessionPersistence = memory
			cfg.AlwaysAllow = []string{"OutputFixture"}
			cfg.MaxResultSizeBytes = 1024
			cfg.MaxToolOutputBytesPerTurn = 1500
			var requests atomic.Int32
			provider := &childRuntimeTestProvider{name: cfg.Model, stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				if requests.Add(1) > 1 {
					return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
				}
				calls := make([]llm.ToolCall, 0, 2)
				for _, id := range []string{"../../escaped", "same-call-id"} {
					calls = append(calls, llm.ToolCall{ID: id, Type: "function", Function: llm.Function{Name: "OutputFixture", Arguments: llm.ArgumentsJSON(`{}`)}})
				}
				return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: calls}, nil
			}}
			models := plugin.NewModelRegistry()
			models.Register(provider)
			models.Route(cfg.Model, provider.Name())
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			full := strings.Repeat("output ", 1024)
			a.services.registry.Register(&codeFixtureTool{name: "OutputFixture", text: full})
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			if view.LastTurn.Status != protocol.TurnSucceeded || requests.Load() != 2 {
				t.Fatalf("tool turn failed: %+v; requests=%d", view.LastTurn, requests.Load())
			}

			count, total := 0, 0
			for _, message := range a.History() {
				if message.Role != messages.RoleTool {
					continue
				}
				count++
				total += len(message.Content)
				if len(message.Content) > cfg.MaxResultSizeBytes {
					t.Fatalf("single result exceeds budget: %d", len(message.Content))
				}
			}
			if count != 2 || total == 0 || total > cfg.MaxToolOutputBytesPerTurn {
				t.Fatalf("projected results=%d bytes=%d", count, total)
			}

			p := a.persistenceHandle()
			records, err := p.Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := p.RequestArtifacts()
			if err != nil {
				t.Fatal(err)
			}
			finished := 0
			for _, record := range records {
				fact, ok := record.Event.(*session.ToolFinished)
				if !ok {
					continue
				}
				finished++
				if fact.Status != "success" || fact.RawOutput == nil {
					t.Fatalf("projection changed execution outcome: %+v", fact)
				}
				raw, err := artifacts.Read(*fact.RawOutput, int64(len(full)))
				if err != nil || string(raw) != full {
					t.Fatalf("complete output was not retained: bytes=%d err=%v", len(raw), err)
				}
			}
			if finished != 2 {
				t.Fatalf("finished calls=%d", finished)
			}
			for _, name := range []string{"escaped.txt", "outputs"} {
				if _, err := os.Stat(filepath.Join(cfg.SessionDir, name)); !os.IsNotExist(err) {
					t.Fatalf("call ID created a legacy output path %q: %v", name, err)
				}
			}
			sessionPath := filepath.Join(cfg.SessionDir, a.SessionID())
			if memory {
				if _, err := os.Stat(cfg.SessionDir); !os.IsNotExist(err) {
					t.Fatalf("memory output created session files: %v", err)
				}
			} else if _, err := os.Stat(sessionPath); err != nil {
				t.Fatalf("missing session-owned storage: %v", err)
			}
		})
	}
}

func TestLoadSessionRejectsTraversalID(t *testing.T) {
	if _, err := archive.Load(t.TempDir(), "../outside"); err == nil {
		t.Fatal("expected traversal session id to be rejected")
	}
}
