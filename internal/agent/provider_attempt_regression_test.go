package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
)

type streamUsageTransport struct{}

func (streamUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body struct {
		Options *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	frames := `data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n"
	if body.Options != nil && body.Options.IncludeUsage {
		frames += `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}` + "\n\n"
	}
	frames += "data: [DONE]\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}, nil
}

func TestChatUsageReachesSessionAndGoal(t *testing.T) {
	a, _, cfg := newJournalAgent(t, true)
	journalGoal(t, a)
	client, e := llm.NewClient(llm.Config{BaseURL: "https://example.invalid/v1", Model: cfg.Model, OneAttempt: true, HTTPClient: &http.Client{Transport: streamUsageTransport{}}})
	if e != nil {
		t.Fatal(e)
	}
	call, e := llm.NewPreparedCall(client, client.Name(), client.Endpoint(), journalRequest())
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: cfg.Clone()}, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	usage := a.Usage()
	a.mu.Lock()
	goal := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if usage.InputTokens != 100 || usage.OutputTokens != 20 || goal.TokensUsed != 80 || goal.TokensEstimated {
		t.Fatalf("standard usage lost: usage=%+v goal=%+v; want input=100 output=20 goal=80 authoritative", usage, goal)
	}
}

type streamEOFTransport struct {
	wire  string
	calls int
}

func (r *streamEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	frames := ""
	if r.calls > 1 {
		if r.wire == "responses" {
			frames = `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
		} else {
			frames = `data: {"type":"message_stop"}` + "\n\n"
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}, nil
}

func TestRuntimeRetriesEmptyWireEOF(t *testing.T) {
	for _, wire := range []string{"responses", "anthropic"} {
		t.Run(wire, func(t *testing.T) {
			a, _, cfg := newJournalAgent(t, true)
			transport := &streamEOFTransport{wire: wire}
			client, e := llm.NewClient(llm.Config{BaseURL: "https://example.invalid/v1", Model: cfg.Model, Wire: wire, OneAttempt: true, HTTPClient: &http.Client{Transport: transport}})
			if e != nil {
				t.Fatal(e)
			}
			call, e := llm.NewPreparedCall(client, client.Name(), client.Endpoint(), journalRequest())
			if e != nil {
				t.Fatal(e)
			}
			_, e = a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: cfg.Clone()}, nil, nil)
			if e != nil || transport.calls != 2 {
				t.Fatalf("runtime stopped before viable retry: requests=%d error=%v", transport.calls, e)
			}
		})
	}
}

func TestStreamPreparedChecksGoalBeforeEachAttempt(t *testing.T) {
	for _, limit := range []string{"tokens", "calls"} {
		t.Run(limit, func(t *testing.T) {
			a, _, cfg := newJournalAgent(t, true)
			journalGoal(t, a)
			if limit == "tokens" {
				_, err := a.changeGoal(func(g *protocol.Goal) (*protocol.Goal, error) { g.TokenBudget = 1; return g, nil })
				if err != nil {
					t.Fatal(err)
				}
			} else {
				a.services.cfg.MaxTurns = 1
			}
			calls := 0
			provider := &childRuntimeTestProvider{name: "goal-retry", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				calls++
				return llm.StreamResult{PromptTokens: 10, PromptReported: true, CompletionReported: true}, &llm.RetryableError{Err: errors.New("temporary"), RetryAfter: time.Millisecond}
			}}
			call, err := llm.NewPreparedCall(provider, provider.Name(), "", journalRequest())
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: cfg.Clone()}, nil, nil)
			if !errors.Is(err, errGoalStopped) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			a.mu.Lock()
			goal := protocol.CloneGoal(a.conversation.durable.Goal)
			a.mu.Unlock()
			if goal.Status != protocol.GoalBudgetLimited || goal.ModelCalls != 1 || goal.TokensUsed != 10 {
				t.Fatalf("goal=%+v", goal)
			}
			if err := a.persistenceFailure(); err != nil {
				t.Fatalf("budget stop poisoned journal: %v", err)
			}
		})
	}
}

func TestStreamPreparedRetryDoesNotApplyPendingGoalEdit(t *testing.T) {
	a, _, cfg := newJournalAgent(t, true)
	journalGoal(t, a)
	_, err := a.changeGoal(func(g *protocol.Goal) (*protocol.Goal, error) { g.PendingObjective = "new objective"; return g, nil })
	if err != nil {
		t.Fatal(err)
	}
	provider := &journalRetryProvider{}
	call, err := llm.NewPreparedCall(provider, provider.Name(), "", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{Config: cfg.Clone()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	goal := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if goal.PendingObjective != "new objective" || goal.Objective != "finish" || provider.calls.Load() != 2 {
		t.Fatalf("retry applied pending step edit: %+v", goal)
	}
}

func TestGoalRuntimeRetryBudgetStopsWithoutTurnFailure(t *testing.T) {
	calls := 0
	provider := &childRuntimeTestProvider{name: "runtime-retry-budget", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		calls++
		return llm.StreamResult{PromptTokens: 10, PromptReported: true, CompletionReported: true}, &llm.RetryableError{Err: errors.New("temporary"), RetryAfter: time.Millisecond}
	}}
	a, _ := newChildRuntimeTestAgent(t, provider)
	budget := int64(1)
	submitGoalTestCommand(t, a, "retry-create", "set", "keep working", &budget)
	view := waitGoalTestState(t, a, protocol.GoalBudgetLimited)
	if calls != 1 || view.Goal.ModelCalls != 1 || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("retry budget result: calls=%d goal=%+v turn=%+v", calls, view.Goal, view.LastTurn)
	}
}
