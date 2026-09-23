package agent

import (
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
	"context"
	"encoding/json"
	"testing"
)

func TestChildViewCompletedSnapshotAndDirectContinuation(t *testing.T) {
	p := &childRuntimeTestProvider{name: "child-view"}
	p.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		text := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				text, _ = m.Content.(string)
			}
		}
		return llm.StreamResult{Text: text, FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "original"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	reader, err := a.Sessions().OpenReader(context.Background(), r.fact.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reader.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Settings.Model.Model != "child-test-model" || view.Settings.Permission.Mode != "bypassPermissions" || view.Closing || view.Busy || view.Phase != protocol.PhaseIdle || len(view.Transcript) == 0 {
		t.Fatalf("completed view lost settings or transcript: %+v", view)
	}
	// Simulate a recovered reader with no retained in-process final snapshot.
	r.mu.Lock()
	savedFinal := r.final
	r.final = protocol.SessionView{}
	r.mu.Unlock()
	recovered, err := reader.Snapshot(context.Background())
	if err != nil || recovered.Settings.Model.Model != view.Settings.Model.Model || recovered.Settings.Permission.Mode != view.Settings.Permission.Mode {
		t.Fatalf("recovered view lost settings: %+v %v", recovered, err)
	}
	r.mu.Lock()
	r.final = savedFinal
	r.mu.Unlock()
	cmd := protocol.NewSubmitInput("direct-followup", view.SessionID, "distinct-input-id", "followup", protocol.InputSteer)
	cmd.ExpectedRunID = view.RunID
	receipt, err := reader.Submit(context.Background(), cmd)
	if err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("continue: %+v %v", receipt, err)
	}
	next, err := a.supervisor.lookup(view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, next)
	if next.err != nil || next.fact.Child.Run.Output != "followup" {
		t.Fatalf("continuation: %+v %v", next.fact.Child.Run, next.err)
	}
	repeated, err := reader.Submit(context.Background(), cmd)
	if err != nil || repeated.Status != protocol.ReceiptScheduled {
		t.Fatalf("retry: %+v %v", repeated, err)
	}
	current, _ := a.supervisor.lookup(view.SessionID)
	if current != next {
		t.Fatal("retry created a second run")
	}
	cmd.Input.Text = "different"
	if _, err := reader.Submit(context.Background(), cmd); err == nil {
		t.Fatal("reused command accepted different text")
	}
	view, err = reader.Snapshot(context.Background())
	if err != nil || view.RunID != next.fact.Child.Run.ID || view.Settings.Model.Model != "child-test-model" {
		t.Fatalf("reader failed to follow continuation: %+v %v", view, err)
	}
}

func TestReasoningOnlyChildCompletesAndRestoresThinking(t *testing.T) {
	p := &childRuntimeTestProvider{name: "reasoning-only"}
	p.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Reasoning: "I need to implement the task", FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "write a file"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	if r.fact.Child.Run.Status != "succeeded" {
		t.Fatalf("empty response marked %s", r.fact.Child.Run.Status)
	}
	reader, err := a.Sessions().OpenReader(context.Background(), r.fact.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reader.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range view.Transcript {
		if item.Kind == "thinking" && item.Text == "I need to implement the task" {
			found = true
		}
	}
	if !found {
		t.Fatal("settled child lost saved reasoning")
	}
	output, err := tools.NewAgentTool().Run(&tools.Context{
		Context: context.Background(), Sessions: a.Sessions(),
		Args: map[string]any{"action": "output", "session_id": string(view.SessionID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var history protocol.TranscriptPage
	if err := json.Unmarshal([]byte(output), &history); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range history.Items {
		if item.Kind != "thinking" {
			continue
		}
		found = true
		page, err := a.Sessions().ReadOutput(context.Background(), view.SessionID, item.ID, 0, 1024)
		if err != nil || page.Text != "I need to implement the task" {
			t.Fatalf("read saved thinking: %+v, %v", page, err)
		}
	}
	if !found {
		t.Fatal("output without item_id did not return saved thinking")
	}
}

func TestReasoningOnlyMainCompletesNormally(t *testing.T) {
	p := &childRuntimeTestProvider{name: "reasoning-only-main"}
	p.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Reasoning: "Still planning the task", FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	go a.Run()
	submitChildRuntimeTestInput(t, a, "reasoning-only-main-input", "implement the task")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("reasoning-only main turn marked %s", view.LastTurn.Status)
	}
	for _, item := range view.Transcript {
		if item.Kind == "error" {
			t.Fatalf("normal completion produced error: %+v", item)
		}
	}
}

func TestProviderLengthLimitContinuesWithoutConfiguredCap(t *testing.T) {
	calls := 0
	p := &childRuntimeTestProvider{name: "provider-length"}
	p.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		calls++
		if calls == 1 {
			return llm.StreamResult{Reasoning: "unfinished thought", FinishReason: "length"}, nil
		}
		return llm.StreamResult{Text: "finished answer", FinishReason: "stop"}, nil
	}
	a, cfg := newChildRuntimeTestAgent(t, p)
	if cfg.ModelConfigFor(cfg.Model).MaxOutputTokens != 0 {
		t.Fatal("test requires no explicit model output cap")
	}
	go a.Run()
	submitChildRuntimeTestInput(t, a, "length-input", "finish task")
	view := waitChildRuntimeTestIdle(t, a)
	if calls != 2 || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("calls=%d outcome=%+v", calls, view.LastTurn)
	}
}
