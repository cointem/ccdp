package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/tools"
)

func TestProcessResumeMarksUnattachedCommandUnknownOnce(t *testing.T) {
	a := newJournalRuntimeAgent(t, &childRuntimeTestProvider{name: "process-resume", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		t.Error("recovery started a model request")
		return llm.StreamResult{}, nil
	}})
	state := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.previous.owner.command", Status: "registered"}, ProcessInfo: protocol.ProcessInfo{Origin: "tool:turn-1:step-1:bash", Command: "cat"}, Revision: 1}
	if r := a.commitProcessFact(processFactRequested{fact: protocol.ProcessEvent{Kind: "registered", State: state}}); r.err != nil {
		t.Fatal(r.err)
	}
	cfg, id := a.configSnapshot(), a.SessionID()
	a.Close()
	saved, err := archive.Load(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(&cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.mu.Lock()
	proc := resumed.conversation.durable.Processes[state.ID]
	resumed.mu.Unlock()
	if proc.State.Status != "unknown" || !proc.State.Released || proc.Result == nil || proc.Projected {
		t.Fatalf("incorrect recovery: %+v", proc)
	}
	before := resumed.persistenceHandle().CurrentCursor()
	if err := resumed.recoverProcesses(); err != nil {
		t.Fatal(err)
	}
	if resumed.persistenceHandle().CurrentCursor() != before {
		t.Fatal("recovery duplicated facts")
	}
}

func TestBashProgressDoesNotAddModelRequests(t *testing.T) {
	f := &fakeLLM{script: []string{"tool:Bash|command=printf first && sleep 0.8 && printf last", "text:done"}}
	a, events := newTestAgent(t, f)
	go a.Run()
	submitTestInput(t, a, "run the test")
	saw := drainUntil(t, events, 15*time.Second, func(ev protocol.EventView) bool { return ev.Kind == protocol.EventTurnDone })
	progress, starts := false, 0
	for _, event := range saw {
		if event.Kind == protocol.EventToolStarted {
			starts++
		}
		if event.Kind == protocol.EventToolProgress && event.Tool != nil && strings.Contains(event.Tool.Output, "first") {
			progress = true
		}
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if !progress || starts != 1 || calls != 2 {
		t.Fatalf("progress=%v tools=%d model_requests=%d", progress, starts, calls)
	}
	for _, message := range a.History() {
		if message.Role == messages.RoleTool {
			var p protocol.ProcessOutput
			if err := json.Unmarshal([]byte(message.Content), &p); err != nil || !p.Terminal() || p.Output != "firstlast" {
				t.Fatalf("model received an unfinished result: %s", message.Content)
			}
		}
	}
}

func TestProcessCompletionIsDurableAndDoesNotStartTurn(t *testing.T) {
	f := &fakeLLM{}
	a, _ := newTestAgent(t, f)
	ctx := &tools.Context{WorkingDir: a.configSnapshot().Workspace}
	a.bindProcessEvents(ctx, messages.ToolCall{ID: "bash", Name: "Bash"}, toolJournalContext{turnID: "turn-1", stepID: "step-1"})
	state := protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test.owner.command", Status: "registered"}, ProcessInfo: protocol.ProcessInfo{Origin: ctx.ProcessOrigin, Command: "build"}, Revision: 1}
	if _, err := ctx.ProcessEvent(protocol.ProcessEvent{Kind: "registered", State: state}); err != nil {
		t.Fatal(err)
	}
	state.Status = "exited"
	state.Output = "done"
	state.ResultID = "result." + state.ID
	state.Revision++
	for i := 0; i < 2; i++ {
		if _, err := ctx.ProcessEvent(protocol.ProcessEvent{Kind: "finished", State: state}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range records {
		switch session.EventPointer(r.Event).(type) {
		case *session.ProcessFinished:
			count++
		case *session.InputQueued:
			t.Fatal("completion created an input")
		}
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if count != 1 || calls != 0 {
		t.Fatalf("finished=%d requests=%d", count, calls)
	}
	a.mu.Lock()
	proc := a.conversation.durable.Processes[state.ID]
	a.mu.Unlock()
	if proc.Projected || proc.Result == nil {
		t.Fatal("completion was lost or consumed before projection")
	}
}
