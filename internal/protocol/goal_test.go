package protocol

import (
	"math"
	"testing"
	"time"
)

func TestGoalCommandNormalization(t *testing.T) {
	budget := int64(42)
	input := GoalCommand{Action: " set ", Objective: " implement feature ", TokenBudget: &budget}
	cmd, err := (Command{ID: "cmd", SessionID: "session", Type: CommandGoal, Goal: &input}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Goal.Action != "set" || cmd.Goal.Objective != "implement feature" {
		t.Fatalf("normalize = %+v", cmd.Goal)
	}
	*cmd.Goal.TokenBudget = 7
	if budget != 42 || input.Objective != " implement feature " {
		t.Fatal("normalization mutated caller")
	}
	for _, action := range []string{"set", "edit", "pause", "resume", "clear", "budget"} {
		goal := &GoalCommand{Action: action}
		if action == "set" || action == "edit" {
			goal.Objective = "work"
		}
		if action == "budget" || action == "resume" {
			goal.TokenBudget = &budget
		}
		if err := (Command{ID: "cmd", SessionID: "s", Type: CommandGoal, Goal: goal}).Validate(); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	for _, goal := range []*GoalCommand{{Action: "unknown"}, {Action: "set"}, {Action: "edit"}, {Action: "budget"}, {Action: "pause", TokenBudget: &budget}, {Action: "clear", Objective: "oops"}} {
		if err := (Command{ID: "cmd", SessionID: "s", Type: CommandGoal, Goal: goal}).Validate(); err == nil {
			t.Fatalf("accepted %+v", goal)
		}
	}
	if err := (Command{ID: "cmd", SessionID: "s", Type: CommandGoal, Goal: &input, Input: &SubmitInput{Text: "extra"}}).Validate(); err == nil {
		t.Fatal("accepted multiple payloads")
	}
	if err := (Command{ID: "cmd", SessionID: "s", Type: CommandQuery, Query: &QueryCommand{Kind: QueryGoal}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGoalValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := Goal{ID: "g", Objective: "work", Status: GoalActive, CreatedAt: now, UpdatedAt: now}
	for _, mutate := range []func(*Goal){func(g *Goal) { g.ID = "" }, func(g *Goal) { g.Objective = " " }, func(g *Goal) { g.Status = "bad" }, func(g *Goal) { g.TokenBudget = -1 }, func(g *Goal) { g.TokensUsed = -1 }, func(g *Goal) { g.RunModelCalls = 1 }, func(g *Goal) { g.ActiveTimeSeconds = math.Inf(1) }, func(g *Goal) { g.UpdatedAt = now.Add(-time.Second) }} {
		goal := valid
		mutate(&goal)
		if err := goal.Validate(); err == nil {
			t.Fatalf("accepted %+v", goal)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	copy := CloneGoal(&valid)
	copy.Objective = "other"
	if valid.Objective != "work" || CloneGoal(nil) != nil {
		t.Fatal("clone aliases original")
	}
}
