package protocol

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type GoalStatus string

const (
	GoalActive        GoalStatus = "active"
	GoalPaused        GoalStatus = "paused"
	GoalBlocked       GoalStatus = "blocked"
	GoalBudgetLimited GoalStatus = "budget_limited"
	GoalComplete      GoalStatus = "complete"
)

// Goal is the complete durable objective state for one session. A zero budget
// means unlimited. RunModelCalls counts calls since the last explicit resume.
type Goal struct {
	ID                string     `json:"id"`
	PendingObjective  string     `json:"pending_objective,omitempty"`
	Objective         string     `json:"objective"`
	Status            GoalStatus `json:"status"`
	Reason            string     `json:"reason,omitempty"`
	TokenBudget       int64      `json:"token_budget,omitempty"`
	TokensEstimated   bool       `json:"tokens_estimated,omitempty"`
	TokensUsed        int64      `json:"tokens_used"`
	ModelCalls        int        `json:"model_calls"`
	RunModelCalls     int        `json:"run_model_calls"`
	ActiveTimeSeconds float64    `json:"active_time_seconds"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func CloneGoal(goal *Goal) *Goal {
	if goal == nil {
		return nil
	}
	copy := *goal
	return &copy
}

func (g Goal) Validate() error {
	if strings.TrimSpace(g.ID) == "" || strings.ContainsAny(g.ID, "\r\n") {
		return errors.New("goal id is required and must not contain newlines")
	}
	if err := validateCommandText("goal objective", g.Objective, MaxSubmitInputTextBytes, true); err != nil {
		return err
	}
	if g.PendingObjective != "" {
		if err := validateCommandText("pending goal objective", g.PendingObjective, MaxSubmitInputTextBytes, true); err != nil {
			return err
		}
	}
	switch g.Status {
	case GoalActive, GoalPaused, GoalBlocked, GoalBudgetLimited, GoalComplete:
	default:
		return fmt.Errorf("unknown goal status %q", g.Status)
	}
	if g.TokenBudget < 0 || g.TokensUsed < 0 || g.ModelCalls < 0 || g.RunModelCalls < 0 || g.RunModelCalls > g.ModelCalls {
		return errors.New("goal counters must be nonnegative and run calls cannot exceed total calls")
	}
	if math.IsNaN(g.ActiveTimeSeconds) || math.IsInf(g.ActiveTimeSeconds, 0) || g.ActiveTimeSeconds < 0 {
		return errors.New("goal active time must be finite and nonnegative")
	}
	if g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() || g.UpdatedAt.Before(g.CreatedAt) {
		return errors.New("goal timestamps are required and updated_at cannot precede created_at")
	}
	return nil
}

type GoalCommand struct {
	Action      string `json:"action"`
	Objective   string `json:"objective,omitempty"`
	TokenBudget *int64 `json:"token_budget,omitempty"`
}

func (c GoalCommand) normalize() (GoalCommand, error) {
	c.Action = strings.TrimSpace(c.Action)
	c.Objective = strings.TrimSpace(c.Objective)
	if c.TokenBudget != nil {
		budget := *c.TokenBudget
		if budget < 0 {
			return GoalCommand{}, errors.New("protocol: goal token budget must be nonnegative")
		}
		c.TokenBudget = &budget
	}
	switch c.Action {
	case "set", "edit":
		if err := validateCommandText("goal objective", c.Objective, MaxSubmitInputTextBytes, true); err != nil {
			return GoalCommand{}, err
		}
	case "budget":
		if c.Objective != "" || c.TokenBudget == nil {
			return GoalCommand{}, errors.New("protocol: goal budget requires only token_budget")
		}
	case "resume":
		if c.Objective != "" {
			return GoalCommand{}, errors.New("protocol: goal resume does not accept objective")
		}
	case "pause", "clear":
		if c.Objective != "" || c.TokenBudget != nil {
			return GoalCommand{}, errors.New("protocol: goal action does not accept objective or token_budget")
		}
	default:
		return GoalCommand{}, fmt.Errorf("protocol: unknown goal action %q", c.Action)
	}
	return c, nil
}
