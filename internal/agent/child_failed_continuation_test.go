package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
)

func TestChildReaderSendsAfterMalformedToolCall(t *testing.T) {
	const reply = "收到"
	var attempts atomic.Int32
	provider := &childRuntimeTestProvider{name: "failed-child-continuation"}
	provider.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		for _, m := range req.Messages {
			if m.Role == "user" && m.Content == "你好" {
				return llm.StreamResult{Text: reply, FinishReason: "stop"}, nil
			}
		}
		attempts.Add(1)
		return llm.StreamResult{
			Reasoning: "searching", FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "incomplete", Type: "function", Function: llm.Function{
				Name: "WebFetch", Arguments: llm.ArgumentsJSON(`{"url":`),
			}}},
		}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	ctx := context.Background()
	child, err := a.services.supervisor.launch(a, a.Context(), childTask{Description: "research", Role: "worker", WorkspaceMode: "isolated"}, childPurposeTask, "research")
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, child)
	row := child.fact.Child
	if attempts.Load() != 2 || row.Run.Status != "failed" || !strings.Contains(row.Run.Error, "invalid tool call") {
		t.Fatalf("expected invalid tool call, got %+v", row.Run)
	}
	reader, err := a.Sessions().OpenReader(ctx, row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reader.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	cmd := protocol.NewSubmitInput("desktop-followup", row.SessionID, "desktop-followup", "你好", protocol.InputSteer)
	cmd.ExpectedRunID = row.Run.ID
	receipt, err := reader.Submit(ctx, cmd)
	if err != nil || receipt.Rejected() {
		t.Fatalf("send failed: %+v %v", receipt, err)
	}
	next, err := a.services.supervisor.lookup(row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, next)
	if next.fact.Child.Run.Status != "succeeded" || next.fact.Child.Run.Output != reply {
		t.Fatalf("continued run: %+v", next.fact.Child.Run)
	}
}

func TestIncompleteChildCallRegeneratedBeforeDispatch(t *testing.T) {
	var requests atomic.Int32
	provider := &childRuntimeTestProvider{name: "truncated-child-call"}
	provider.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		call := llm.ToolCall{ID: "child", Type: "function", Function: llm.Function{Name: "JoinChildTest", Arguments: llm.ArgumentsJSON(`{"description":"research"}`)}}
		switch n := requests.Add(1); n {
		case 1:
			broken := call
			broken.ID, broken.Function.Arguments = "broken", llm.ArgumentsJSON(`{"description":`)
			return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call, broken}}, nil
		case 2:
			last := req.Messages[len(req.Messages)-1]
			hint, _ := last.Content.(string)
			if last.Role != "user" || !strings.Contains(hint, "unexpected EOF") || !strings.Contains(hint, "Reissue") {
				return llm.StreamResult{}, errors.New("missing repair instruction")
			}
			for _, m := range req.Messages {
				if len(m.ToolCalls) > 0 || m.Role == "tool" {
					return llm.StreamResult{}, errors.New("broken batch replayed")
				}
			}
			return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}}, nil
		case 3:
			return llm.StreamResult{Text: "research complete", FinishReason: "stop"}, nil
		case 4:
			for _, m := range req.Messages {
				if m.Role == "tool" && strings.Contains(fmt.Sprint(m.Content), "research complete") {
					return llm.StreamResult{Text: "parent complete", FinishReason: "stop"}, nil
				}
			}
		}
		return llm.StreamResult{}, errors.New("unexpected request")
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	submitChildRuntimeTestInput(t, a, "request-research", "delegate research")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status != protocol.TurnSucceeded || requests.Load() != 4 || view.History[len(view.History)-1].Content != "parent complete" {
		t.Fatalf("turn=%+v requests=%d", view.LastTurn, requests.Load())
	}
	for _, item := range view.Transcript {
		if item.Kind == "error" || item.CallID == "broken" || strings.Contains(item.Text, "Reissue the needed calls") {
			t.Fatalf("invalid batch or repair leaked into transcript: %+v", item)
		}
	}
}
