package tools

import (
	"ccdp/internal/protocol"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

type goalControlStub struct {
	goal      *protocol.Goal
	err       error
	calls     int
	objective string
	budget    *int64
	status    string
	reason    string
}

func (s *goalControlStub) GetGoal() (*protocol.Goal, error) {
	s.calls++
	return s.goal, s.err
}
func (s *goalControlStub) CreateGoal(objective string, budget *int64) (*protocol.Goal, error) {
	s.calls++
	s.objective, s.budget = objective, budget
	return s.goal, s.err
}
func (s *goalControlStub) UpdateGoal(status, reason string) (*protocol.Goal, error) {
	s.calls++
	s.status, s.reason = status, reason
	return s.goal, s.err
}

func TestGoalToolsRoundTripAndMissingGoal(t *testing.T) {
	stub := &goalControlStub{goal: &protocol.Goal{ID: "goal-1", Objective: "Finish task", Status: protocol.GoalActive}}
	ctx := &Context{Goals: stub, Args: map[string]any{"objective": " Finish task ", "token_budget": json.Number("12345")}}
	out, err := NewCreateGoalTool().Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stub.objective != "Finish task" || stub.budget == nil || *stub.budget != 12345 {
		t.Fatalf("unexpected create: %+v", stub)
	}
	var got protocol.Goal
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != stub.goal.ID {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	ctx.Args = map[string]any{"objective": "Finish task"}
	if _, err := NewCreateGoalTool().Run(ctx); err != nil || stub.budget != nil {
		t.Fatalf("omitted budget = %v, err = %v", stub.budget, err)
	}
	ctx.Args = nil
	stub.goal = nil
	if out, err := NewGetGoalTool().Run(ctx); err != nil || out != "null" {
		t.Fatalf("missing goal = %q, err = %v", out, err)
	}
	for _, status := range []string{"complete", "blocked", "paused"} {
		ctx.Args = map[string]any{"status": status, "reason": " verified reason "}
		if _, err := NewUpdateGoalTool().Run(ctx); err != nil {
			t.Fatal(err)
		}
		if stub.status != status || stub.reason != "verified reason" {
			t.Fatalf("unexpected update: %+v", stub)
		}
	}
}

func TestGoalToolsRejectInvalidInputBeforeControl(t *testing.T) {
	for _, budget := range []any{nil, 0, -1, 0.5, "100", json.Number("1.5"), json.Number("9223372036854775808"), math.Inf(1), math.NaN()} {
		stub := &goalControlStub{}
		if _, err := NewCreateGoalTool().Run(&Context{Goals: stub, Args: map[string]any{"objective": "task", "token_budget": budget}}); err == nil || stub.calls != 0 {
			t.Errorf("invalid budget %v: err=%v calls=%d", budget, err, stub.calls)
		}
	}
	for _, tc := range []struct {
		tool Tool
		args map[string]any
	}{
		{NewCreateGoalTool(), nil},
		{NewCreateGoalTool(), map[string]any{"objective": " \n "}},
		{NewCreateGoalTool(), map[string]any{"objective": 42}},
		{NewGetGoalTool(), map[string]any{"resume": true}},
		{NewUpdateGoalTool(), map[string]any{"status": "active", "reason": "resume"}},
		{NewUpdateGoalTool(), map[string]any{"status": "budget_limited", "reason": "limit"}},
		{NewUpdateGoalTool(), map[string]any{"status": "complete"}},
		{NewUpdateGoalTool(), map[string]any{"status": "blocked", "reason": " "}},
		{NewUpdateGoalTool(), map[string]any{"status": "paused", "reason": 42}},
		{NewUpdateGoalTool(), map[string]any{"status": "complete", "reason": "done", "token_budget": 999}},
	} {
		stub := &goalControlStub{}
		if _, err := tc.tool.Run(&Context{Goals: stub, Args: tc.args}); err == nil || stub.calls != 0 {
			t.Errorf("%s accepted %+v: err=%v calls=%d", tc.tool.Name(), tc.args, err, stub.calls)
		}
	}
}

func TestGoalToolsRespectInvocationScopeAndErrors(t *testing.T) {
	errDenied := errors.New("child cannot mutate parent goal")
	stub := &goalControlStub{err: errDenied}
	args := map[string]any{"status": "complete", "reason": "verified"}
	if _, err := NewUpdateGoalTool().Run(&Context{Goals: stub, Args: args}); !errors.Is(err, errDenied) {
		t.Fatalf("lost controller error: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	stub.calls = 0
	if _, err := NewUpdateGoalTool().Run(&Context{Context: canceled, Goals: stub, Args: args}); !errors.Is(err, context.Canceled) || stub.calls != 0 {
		t.Fatalf("canceled invocation: %v calls=%d", err, stub.calls)
	}
	resources := NewResources("owner", t.TempDir())
	t.Cleanup(func() { _ = resources.Close() })
	if _, err := NewUpdateGoalTool().Run(&Context{Resources: resources, Owner: "other", Goals: stub, Args: args}); err == nil || stub.calls != 0 {
		t.Fatalf("cross-session invocation: %v calls=%d", err, stub.calls)
	}
	for _, ctx := range []*Context{nil, {}} {
		if _, err := NewGetGoalTool().Run(ctx); err == nil {
			t.Fatal("missing goal capability should fail")
		}
	}
}
