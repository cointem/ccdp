package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/codemode"
	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestCodeResultProjectionPreservesExecution(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		limit      int
		wantError  bool
	}{
		{"small_budget", `text(1)`, 1024, false},
		{"many_values", `for(let i=0;i<20000;i++) text(1); await tools.Write({file_path:"result.txt",content:"once"});`, 32000, false},
		{"escaped_error", `text("x".repeat(27500)); throw new Error("<".repeat(2000));`, 32000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls, events := codeAgent(t, tc.code, func(cfg *config.Config) {
				cfg.MaxResultSizeBytes = tc.limit
			})
			finishCode(t, a, events)
			if calls.Load() != 2 {
				t.Fatalf("model calls = %d; history: %+v", calls.Load(), a.History())
			}
			var output string
			for _, m := range a.History() {
				if m.Role == messages.RoleTool && m.ToolCallID == "code-parent" {
					if len(m.Content) > tc.limit {
						t.Fatalf("model message exceeds budget: %d > %d", len(m.Content), tc.limit)
					}
					output = strings.TrimPrefix(m.Content, "Error: ")
				}
			}
			var result protocol.CodeResult
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatalf("invalid model result: %v: %s", err, output)
			}
			if len(output) > tc.limit || (result.Error != "") != tc.wantError {
				t.Fatalf("bytes=%d, error=%q", len(output), result.Error)
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			var finished *session.ToolFinished
			for _, record := range records {
				if e, ok := record.Event.(*session.ToolFinished); ok && e.CallID == "code-parent" {
					finished = e
				}
			}
			wantStatus := "success"
			if tc.wantError {
				wantStatus = "error"
			}
			if finished == nil || finished.Status != wantStatus {
				t.Fatalf("execution status lost: %+v", finished)
			}
			if tc.name == "many_values" {
				if result.Calls != 1 || result.Note == "" || len(result.Outputs) >= 20000 {
					t.Fatalf("missing summary or omission: calls=%d, note=%q, outputs=%d", result.Calls, result.Note, len(result.Outputs))
				}
				var original protocol.CodeResult
				if err := json.Unmarshal([]byte(finished.Result.Text), &original); err != nil || len(original.Outputs) != 20000 {
					t.Fatalf("projection modified raw execution: outputs=%d err=%v", len(original.Outputs), err)
				}
				data, err := os.ReadFile(filepath.Join(a.services.cfg.Workspace, "result.txt"))
				if err != nil || string(data) != "once" {
					t.Fatalf("write after text output: %q, %v", data, err)
				}
			}
		})
	}
}

func TestCodeResultIncludesHookContext(t *testing.T) {
	a, _, events := codeAgent(t, `text(1)`, nil)
	a.services.gohooks.AddPostTool(func(name string, _ map[string]any, _ string) string {
		if name == "RunCode" {
			return "hook context"
		}
		return ""
	})
	finishCode(t, a, events)
	for _, message := range a.History() {
		if message.Role != messages.RoleTool {
			continue
		}
		var result protocol.CodeResult
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil || !strings.Contains(result.Note, "hook context") {
			t.Fatalf("hook broke structured result: %v: %s", err, message.Content)
		}
		return
	}
	t.Fatal("missing tool result")
}

func TestCodeResultExhaustedBudgetContinuesWithoutRepeatingEffects(t *testing.T) {
	a, calls, events := codeAgent(t, `await tools.Write({file_path:"result.txt",content:"once"}); text(1)`, func(cfg *config.Config) {
		cfg.MaxToolOutputBytesPerTurn = 32
	})
	finishCode(t, a, events)
	view, err := a.Snapshot(context.Background())
	if err != nil || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("turn failed after output budget exhaustion: %+v, %v", view.LastTurn, err)
	}
	if calls.Load() != 2 || a.persistenceFailure() != nil {
		t.Fatalf("did not continue inference or poisoned session: calls=%d, persistence=%v", calls.Load(), a.persistenceFailure())
	}
	data, err := os.ReadFile(filepath.Join(a.services.cfg.Workspace, "result.txt"))
	if err != nil || string(data) != "once" {
		t.Fatalf("lost successful write: %q, %v", data, err)
	}
	// A later explicit turn must not contain an orphaned assistant tool call.
	submitTestInput(t, a, "continue")
	drainUntil(t, events, 20*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
	if calls.Load() != 3 {
		t.Fatalf("next turn was not usable: calls=%d", calls.Load())
	}
	pending := map[string]bool{}
	for _, message := range a.History() {
		for _, call := range message.ToolCalls {
			pending[call.ID] = true
		}
		if message.Role == messages.RoleTool {
			delete(pending, message.ToolCallID)
		}
		if message.Role == messages.RoleUser && len(pending) != 0 {
			t.Fatalf("new input follows unpaired tool calls: %v", pending)
		}
	}
	if len(pending) != 0 {
		t.Fatalf("unpaired tool calls: %v", pending)
	}
}

func TestCodeModeArgumentPermissionKeepsBinding(t *testing.T) {
	a, _, _ := codeAgent(t, "", func(cfg *config.Config) {
		cfg.PermissionMode = string(permissions.ModeManual)
		cfg.AlwaysAllow = []string{"Write:result.txt"}
	})
	a.children.childState = &childRuntimeState{
		purpose: childPurposeTask, nonInteractive: true,
		allowed: map[string]bool{"Write": true}, perms: a.services.perms,
	}
	lease := a.services.registry.Acquire()
	jc := toolJournalContextForStep(testToolStep(t, a))
	jc.toolLease, jc.allowedTools = lease, []string{"Write"}
	result, err := a.runCode(context.Background(), messages.ToolCall{ID: "limited"},
		jc,
		`text(typeof tools.Write); await tools.Write({file_path:"result.txt",content:"allowed"}); try {await tools.Write({file_path:"other.txt",content:"denied"})} catch(e) {text(e.status)}`, 5*time.Second)
	if err != nil || result.Calls != 2 || result.Denied != 1 || len(result.Outputs) != 2 || string(result.Outputs[0]) != `"function"` || string(result.Outputs[1]) != `"denied"` {
		t.Fatalf("binding/permission result: %+v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(a.services.cfg.Workspace, "result.txt")); err != nil || string(data) != "allowed" {
		t.Fatalf("allowed write: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(a.services.cfg.Workspace, "other.txt")); !os.IsNotExist(err) {
		t.Fatalf("unauthorized path exists: %v", err)
	}
}

func TestCodeModeUnavailableIsNotAdvertised(t *testing.T) {
	if codemode.Available {
		t.Skip("requires an unsupported platform")
	}
	a := newRuntimeAgent(t)
	if _, ok := a.services.registry.Get("RunCode"); ok {
		t.Fatal("unavailable RunCode was registered")
	}
}
