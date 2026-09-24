package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func clarificationQuestions() []protocol.Question {
	return []protocol.Question{{Question: "Which database?", Header: "Database", Options: []protocol.QuestionOption{{Label: "SQLite", Description: "Local"}, {Label: "Postgres", Description: "Server"}}}}
}

func TestQuestionRoundTripAndReasoningWire(t *testing.T) {
	var calls atomic.Int32
	requests := make(chan llm.CompletionRequest, 4)
	args, _ := json.Marshal(map[string]any{"questions": clarificationQuestions()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		requests <- req
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "Need database clarification", "tool_calls": []any{map[string]any{"index": 0, "id": "ask-1", "type": "function", "function": map[string]any{"name": "AskUserQuestion", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}})
			writeSSE(w, string(chunk))
		} else {
			writeSSE(w, `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		}
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.Workspace, cfg.SessionDir = t.TempDir(), t.TempDir()
	cfg.BaseURL, cfg.Model = server.URL, "deepseek-test"
	cfg.ReasoningEffort, cfg.Verbosity = "high", "low"
	events := make(chan Event, 128)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	go ag.Run()
	submitTestInput(t, ag, "Build a database app")
	seen := drainUntil(t, events, 5*time.Second, func(ev Event) bool { return ev.Type == EventQuestion })
	request := seen[len(seen)-1].Question
	if request == nil {
		t.Fatal("missing question")
	}
	view, _ := ag.Snapshot(context.Background())
	if view.Question == nil || view.Approval != nil {
		t.Fatal("question must have its own snapshot slot")
	}
	view.Question.Questions[0].Options[0].Label = "corrupted"
	submitAnswer := func(id string, answer protocol.AnswerQuestion) protocol.Receipt {
		return submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(id), SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandAnswerQuestion, Answer: &answer})
	}
	if !submitAnswer("stale", protocol.AnswerQuestion{RequestID: "stale", Cancelled: true}).Rejected() {
		t.Fatal("stale answer accepted")
	}
	invalid := protocol.AnswerQuestion{RequestID: request.ID, Answers: []protocol.QuestionAnswer{{Selected: []string{"invalid"}}}}
	if !submitAnswer("invalid", invalid).Rejected() {
		t.Fatal("invalid answer accepted")
	}
	answer := protocol.AnswerQuestion{RequestID: request.ID, Answers: []protocol.QuestionAnswer{{Selected: []string{"SQLite"}, Text: "离线使用"}}}
	if receipt := submitAnswer("answer", answer); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	if !submitAnswer("duplicate", answer).Rejected() {
		t.Fatal("duplicate answer accepted")
	}
	drainUntil(t, events, 5*time.Second, func(ev Event) bool { return ev.Type == EventTurnDone })
	first, second := <-requests, <-requests
	if first.ReasoningEffort != "high" || first.Verbosity != "low" || first.Thinking == nil || first.Thinking.Type != "enabled" {
		t.Fatalf("missing generation controls: %+v", first)
	}
	var foundReasoning, foundAnswer bool
	for _, msg := range second.Messages {
		if msg.Role == "assistant" && msg.ReasoningContent == "Need database clarification" {
			foundReasoning = true
		}
		if msg.Role == "tool" {
			text, _ := msg.Content.(string)
			foundAnswer = strings.Contains(text, "SQLite") && strings.Contains(text, "离线使用")
		}
	}
	if !foundReasoning || !foundAnswer {
		t.Fatalf("second request lost reasoning/answer: %+v", second.Messages)
	}
	// The durable message round trip also preserves provider reasoning.
	for _, msg := range ag.history {
		if msg.ReasoningContent == "" {
			continue
		}
		typed, err := messageToSession(msg)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := messageFromSession(typed)
		if err != nil || restored.ReasoningContent != msg.ReasoningContent {
			t.Fatalf("reasoning replay: %+v %v", restored, err)
		}
	}
}

func TestQuestionPlanCapabilityAndCancellation(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()
	tool := &askUserQuestionTool{ag: ag}
	if !planAllowedImplementation(tool) || readOnlyImplementation(tool) {
		t.Fatal("question must be a plan-allowed serial barrier")
	}
	if decision, _ := ag.perms.CheckExecution(permissions.ExecutionModePlan, tool.Name(), nil); decision != permissions.DecisionAllow {
		t.Fatal("plan mode denied clarification")
	}
	raw, _ := json.Marshal(map[string]any{"questions": clarificationQuestions()})
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := tool.Run(&tools.Context{Context: ctx, Args: args}); result <- err }()
	deadline := time.After(time.Second)
	for {
		snapshot, _ := ag.Snapshot(context.Background())
		if snapshot.Question != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("question was not published")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled question blocked")
	}
	snapshot, _ := ag.Snapshot(context.Background())
	if snapshot.Question != nil {
		t.Fatal("cancelled question still pending")
	}
}

func TestGenerationSettingsPersistResetAndFreeze(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()
	go ag.Run()
	effort, verbosity := "max", "low"
	update := func() protocol.Receipt {
		return submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("generation")), SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort, Verbosity: &verbosity}})
	}
	if receipt := update(); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	snapshot, _ := ag.Snapshot(context.Background())
	if snapshot.Settings.ReasoningEffort != "max" || snapshot.Settings.Verbosity != "low" {
		t.Fatal("settings not published")
	}
	frozen := cloneConfig(ag.cfg)
	req := ag.buildRequestFromConfig(frozen, "openai-test", "test", nil, false, nil)
	if req.ReasoningEffort != "max" || req.Verbosity != "low" {
		t.Fatal("request lost settings")
	}
	effort, verbosity = "", ""
	if receipt := update(); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	ag.mu.Lock()
	settings := ag.sessionSettingsLocked()
	ag.mu.Unlock()
	restored := frozen
	applySessionSettingsToConfig(&restored, settings)
	if restored.ReasoningEffort != "" || restored.Verbosity != "" {
		t.Fatal("explicit default not restored")
	}
	if req.ReasoningEffort != "max" {
		t.Fatal("prepared request mutated")
	}
	req = ag.buildRequestFromConfig(restored, "openai-test", "test", nil, false, nil)
	body, _ := json.Marshal(req)
	if req.ReasoningEffort != config.DefaultReasoningEffort {
		t.Fatalf("reset effort = %q, want built-in default %q", req.ReasoningEffort, config.DefaultReasoningEffort)
	}
	if strings.Contains(string(body), "verbosity") {
		t.Fatalf("verbosity must stay omitted when unset: %s", body)
	}
	restored.ReasoningEffort = "none"
	req = ag.buildRequestFromConfig(restored, "deepseek-test", "test", []messages.Message{{Role: messages.RoleAssistant, Content: "ok", ReasoningContent: "reason"}}, false, nil)
	if req.Thinking == nil || req.Thinking.Type != "disabled" || req.ReasoningEffort != "" {
		t.Fatal("DeepSeek none must disable thinking")
	}
}
