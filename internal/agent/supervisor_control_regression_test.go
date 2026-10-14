package agent

import (
	"context"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func TestSupervisorTypedContinuationRetry(t *testing.T) {
	p := &childRuntimeTestProvider{name: "review-retry", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "ok", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.services.supervisor.launch(a, a.lifecycle.rootCtx, childTask{Description: "original"}, childPurposeTask, "call")
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	cmd := protocol.AgentControl{ID: "typed-retry", SessionID: r.fact.Child.SessionID, Action: "continue", Input: &protocol.SubmitInput{ID: "next", Text: "next"}}
	first, err := a.Sessions().Control(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	nr, _ := a.services.supervisor.lookup(r.fact.Child.SessionID)
	waitManaged(t, nr)
	again, err := a.Sessions().Control(context.Background(), cmd)
	if err != nil || again.ID != first.ID {
		t.Fatalf("identical typed continuation retry rejected: %v", err)
	}
	canonical := cmd
	canonical.Text = "next"
	canonical.Input = &protocol.SubmitInput{ID: "next", Text: "next", Strategy: protocol.InputSteer}
	again, err = a.Sessions().Control(context.Background(), canonical)
	if err != nil || again.ID != first.ID {
		t.Fatalf("canonical continuation retry rejected: %v", err)
	}
	changed := cmd
	changed.Input = &protocol.SubmitInput{ID: "next", Text: "different"}
	if _, err = a.Sessions().Control(context.Background(), changed); err == nil {
		t.Fatal("conflicting continuation accepted")
	}
	if cmd.Input.Strategy != "" || cmd.Text != "" {
		t.Fatal("normalization mutated caller input")
	}
	// Rebuild the directory from durable records: retries must use the same
	// normalized identity even when the original in-memory run is gone.
	restored := newSessionSupervisor(a)
	again, err = restored.Control(context.Background(), cmd)
	if err != nil || again.ID != first.ID {
		t.Fatalf("restored continuation retry rejected: %v", err)
	}
}

func TestChildForkRetainsCompletedToolRound(t *testing.T) {
	p := &childRuntimeTestProvider{name: "review-fork", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "ok", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.mu.Lock()
	a.conversation.durable.History = []messages.Message{
		{Role: messages.RoleUser, Content: "inspect project"},
		{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "read-1", Name: "Read", Arguments: map[string]any{"path": "README.md"}}}},
		{Role: messages.RoleTool, ToolCallID: "read-1", Content: "important existing evidence"},
		{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "pending", Name: "SpawnAgent", Arguments: map[string]any{"task": "continue investigation"}}}},
	}
	a.mu.Unlock()
	rows, err := runTestChildren(a, []childTask{{Description: "continue investigation", ContextMode: "fork"}}, "fork")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	saved, err := LoadSession(a.services.cfg.SessionDir, rows[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	foundResult := false
	for _, m := range saved.History {
		if m.ToolCallID == "read-1" {
			foundResult = true
		}
		for _, call := range m.ToolCalls {
			if call.ID == "pending" {
				t.Fatal("fork inherited the parent's unfinished spawn")
			}
		}
	}
	if !foundResult {
		t.Fatalf("fork discarded completed Read round: %+v", saved.History)
	}
}
