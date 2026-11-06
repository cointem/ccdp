package agent

import (
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessCompletionAndWaitAppearOnceInProviderRequest(t *testing.T) {
	var a *Agent
	requests := 0
	counts := make(chan int, 1)
	const sentinel = "UNIQUE_BACKGROUND_COMPLETION_PAYLOAD"
	provider := &childRuntimeTestProvider{name: "review-duplicate", stream: func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		requests++
		call := func(name, args string) llm.StreamResult {
			return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("probe-%d", requests), Type: "function", Function: llm.Function{Name: name, Arguments: llm.ArgumentsJSON(args)}}}, FinishReason: "tool_calls"}
		}
		if requests == 1 {
			return call("Bash", `{"command":"cat","stdin":"pipe"}`), nil
		}
		if requests == 2 {
			var process protocol.ProcessOutput
			for _, message := range a.History() {
				if message.Role == messages.RoleTool {
					_ = json.Unmarshal([]byte(message.Content), &process)
				}
			}
			if process.ID == "" {
				return llm.StreamResult{}, fmt.Errorf("missing process handle")
			}
			toolCtx := a.services.resources.Context(ctx, a.configSnapshot().Workspace, a.currentSandbox())
			toolCtx.Args = map[string]any{"id": process.ID, "action": "write", "input": sentinel, "eof": true}
			if _, err := executeToolText(tools.NewProcessTool(), toolCtx); err != nil {
				return llm.StreamResult{}, err
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				a.mu.Lock()
				pending := a.conversation.durable.Processes[process.ID]
				a.mu.Unlock()
				if pending.Result != nil {
					return call("Process", fmt.Sprintf(`{"id":%q,"action":"wait"}`, process.ID)), nil
				}
				time.Sleep(time.Millisecond)
			}
			return llm.StreamResult{}, fmt.Errorf("completion was not queued")
		}
		encoded, _ := json.Marshal(req.Messages)
		counts <- strings.Count(string(encoded), sentinel)
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	a = newJournalRuntimeAgent(t, provider)
	a.services.cfg.MaxTurns = 4
	events := watchTestEvents(t, a)
	submitJournalRuntimeInput(t, a)
	drainUntil(t, events, 15*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
	select {
	case count := <-counts:
		if count != 1 {
			t.Fatalf("same completion payload appeared %d times in the next model request", count)
		}
	default:
		t.Fatal("model did not reach verification request")
	}
}

func TestProcessProjectionFailureIsAtomic(t *testing.T) {
	for _, failure := range []session.EventType{session.EventTypeProcessRegistered, session.EventTypeProcessStarted, session.EventTypeProcessFinished, session.EventTypeProcessResultsProjected} {
		t.Run(string(failure), func(t *testing.T) {
			var calls atomic.Int32
			provider := &childRuntimeTestProvider{name: "process-fault", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				if calls.Add(1) > 1 {
					return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
				}
				return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "command", Type: "function", Function: llm.Function{Name: "Bash", Arguments: llm.ArgumentsJSON(`{"command":"printf committed > marker; printf payload"}`)}}}, FinishReason: "tool_calls"}, nil
			}}
			a := newJournalRuntimeAgent(t, provider)
			backend := journalBackend(t, a.persistenceHandle())
			backend.mu.Lock()
			backend.store = requestJournalFailEventStore{Store: backend.store, event: failure}
			backend.mu.Unlock()
			submitJournalRuntimeInput(t, a)
			view := waitJournalRuntimeIdle(t, a)
			if view.LastTurn.Status != protocol.TurnFailed || calls.Load() != 1 {
				t.Fatalf("failed commit continued: %+v requests=%d", view.LastTurn, calls.Load())
			}
			if failure == session.EventTypeProcessRegistered {
				if _, err := os.Stat(filepath.Join(a.configSnapshot().Workspace, "marker")); !os.IsNotExist(err) {
					t.Fatal("dispatch preceded durable registration", err)
				}
			}
			a.mu.Lock()
			state := a.conversation.durable.Clone()
			a.mu.Unlock()
			for _, p := range state.Processes {
				if p.Projected {
					t.Fatal("failed batch consumed result")
				}
				if failure == session.EventTypeProcessResultsProjected && p.Result == nil {
					t.Fatal("raw completion lost")
				}
			}
			rows, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Event.Type() == session.EventTypeToolResultsProjected {
					t.Fatal("partial projection committed before delivery failure")
				}
			}
		})
	}
}

func TestProcessPendingBudgetCancellationAndRepeatedWait(t *testing.T) {
	a := newJournalRuntimeAgent(t, &childRuntimeTestProvider{name: "process-budget", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		t.Error("unexpected model request")
		return llm.StreamResult{}, nil
	}})
	state := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.budget.owner.command", Status: "registered"}, ProcessInfo: protocol.ProcessInfo{Origin: "tool:turn-1:step-1:command"}, Revision: 1}
	if r := a.commitProcessFact(processFactRequested{fact: protocol.ProcessEvent{Kind: "registered", State: state}}); r.err != nil {
		t.Fatal(r.err)
	}
	state.Status = "exited"
	state.ResultID = "result." + state.ID
	state.Output = "ONE_AUTOMATIC_PAYLOAD"
	state.Revision++
	if r := a.commitProcessFact(processFactRequested{fact: protocol.ProcessEvent{Kind: "finished", State: state}}); r.err != nil {
		t.Fatal(r.err)
	}
	a.mu.Lock()
	a.services.cfg.MaxToolOutputBytesPerTurn = 4
	a.mu.Unlock()
	if err := a.projectPendingProcesses(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	p := a.conversation.durable.Processes[state.ID]
	a.mu.Unlock()
	if p.Projected {
		t.Fatal("unrepresentable result was consumed")
	}
	a.mu.Lock()
	a.services.cfg.MaxToolOutputBytesPerTurn = 64000
	a.turn.interruptFlag = true
	a.mu.Unlock()
	if err := a.projectPendingProcesses(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	p = a.conversation.durable.Processes[state.ID]
	a.turn.interruptFlag = false
	a.mu.Unlock()
	if p.Projected {
		t.Fatal("cancelled boundary consumed result")
	}
	if err := a.projectPendingProcesses(); err != nil {
		t.Fatal(err)
	}
	if err := a.projectPendingProcesses(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	p = a.conversation.durable.Processes[state.ID]
	a.mu.Unlock()
	if !p.Projected {
		t.Fatal("pending result never delivered")
	}
	calls := []messages.ToolCall{{ID: "wait", Name: "Process"}}
	result := tooling.Result{Process: &p.State.ProcessOutput, CompletionIDs: []string{state.ID}, Status: "success"}
	projected := a.projectToolsFromWorker(toolJournalContext{turnID: "turn-2", stepID: "step-2", maxOutputBytes: 64000}, calls, []tooling.Result{result})
	if projected.err != nil {
		t.Fatal(projected.err)
	}
	if strings.Contains(projected.results[0].Output, state.Output) || !strings.Contains(projected.results[0].Output, p.State.OutputID) {
		t.Fatal("repeat wait lost reference or duplicated payload", projected.results)
	}
	count := 0
	for _, m := range a.History() {
		count += strings.Count(m.Content, state.Output)
	}
	if count != 1 {
		t.Fatalf("automatic delivery count=%d", count)
	}
	page, err := a.readOutput(context.Background(), p.State.OutputID, 0, 8192)
	if err != nil || !strings.Contains(page.Text, state.Output) {
		t.Fatalf("durable result unreadable: %+v %v", page, err)
	}
}

func TestCodeModeProcessCompletionRetainsHostReferencesWithoutEmit(t *testing.T) {
	for _, code := range []string{`await tools.Bash({command:"printf code-data"});`, `const r=await tools.Bash({command:"printf code-data"}); text(r.data.output); text({process_id:"forged",result_id:"forged"}); throw Error("after execution");`} {
		t.Run(fmt.Sprintf("emit-%t", strings.Contains(code, "text(")), func(t *testing.T) {
			a, _, _ := codeAgent(t, `text("unused")`, nil)
			result := a.executeToolResultWithJournal(messages.ToolCall{ID: "run", Name: "RunCode", Arguments: map[string]any{"code": code}}, toolJournalContextForStep(testToolStep(t, a)))
			if result.Code == nil || len(result.Code.Completed) != 1 || result.Code.Completed[0].ID == "forged" || result.Code.Completed[0].OutputID == "" {
				t.Fatalf("missing host-owned completion: %+v", result)
			}
			if strings.Contains(code, "text(") && (len(result.Code.Outputs) < 1 || string(result.Code.Outputs[0]) != `"code-data"`) {
				t.Fatal("script lost real child data", result.Code)
			}
			a.mu.Lock()
			p := a.conversation.durable.Processes[result.Code.Completed[0].ID]
			a.mu.Unlock()
			if p.Projected {
				t.Fatal("script execution consumed completion before outer projection")
			}
		})
	}
}
