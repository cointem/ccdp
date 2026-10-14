package tools

import (
	"ccdp/internal/protocol"
	"encoding/json"
	"fmt"
	"strings"
)

// GoalControl is supplied by the runtime for the invoking session. It owns
// persistence, goal identity checks, accounting and continuation scheduling.
type GoalControl interface {
	GetGoal() (*protocol.Goal, error)
	CreateGoal(objective string, tokenBudget *int64) (*protocol.Goal, error)
	UpdateGoal(status, reason string) (*protocol.Goal, error)
}

type CreateGoalTool struct{}
type GetGoalTool struct{}
type UpdateGoalTool struct{}

func NewCreateGoalTool() *CreateGoalTool { return &CreateGoalTool{} }
func NewGetGoalTool() *GetGoalTool       { return &GetGoalTool{} }
func NewUpdateGoalTool() *UpdateGoalTool { return &UpdateGoalTool{} }

func (*CreateGoalTool) Name() string { return "CreateGoal" }
func (*GetGoalTool) Name() string    { return "GetGoal" }
func (*UpdateGoalTool) Name() string { return "UpdateGoal" }

func (*CreateGoalTool) Description() string {
	return `Create a persistent goal and keep working until it is complete, paused, blocked or limited. Use only when the user or governing instructions explicitly request a persistent goal; do not infer one from an ordinary task. Supply token_budget only when a token budget was explicitly requested. An unfinished goal cannot be replaced. This does not grant any additional permissions or bypass plan approval.`
}

func (*GetGoalTool) Description() string {
	return `Read the current session's goal, status, stop reason and accumulated usage. A missing goal is returned as null. This does not start or resume work.`
}

func (*UpdateGoalTool) Description() string {
	return `Update the current goal's status. Mark complete only after all requested work is finished; give concrete validation or delivery evidence in reason. Mark blocked only after verifying that no useful work can proceed without user input or an external change; explain the blocker and the required change. A long or difficult task is not a blocker. Mark paused only at the user's explicit request. This cannot edit the objective, increase a budget or resume work. After pausing or blocking, report the reason and stop goal work.`
}

func (*CreateGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"objective":    map[string]any{"type": "string", "minLength": 1, "description": "The concrete result the user explicitly asked to pursue as a persistent goal."},
			"token_budget": map[string]any{"type": "integer", "minimum": 1, "description": "Optional positive token budget, only when explicitly requested. Omit for no goal-specific token limit."},
		},
		"required": []string{"objective"},
	}
}

func (*GetGoalTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func (*UpdateGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"complete", "blocked", "paused"}},
			"reason": map[string]any{"type": "string", "minLength": 1, "description": "Completion evidence, a verified blocker and what would resolve it, or the user's pause request."},
		},
		"required": []string{"status", "reason"},
	}
}

func (*CreateGoalTool) Run(ctx *Context) (string, error) {
	controller, err := goalController(ctx, "objective", "token_budget")
	if err != nil {
		return "", err
	}
	objective := strings.TrimSpace(StringArg(ctx.Args, "objective", ""))
	if objective == "" {
		return "", fmt.Errorf("CreateGoal: objective must be a non-empty string")
	}
	var budget *int64
	if raw, supplied := ctx.Args["token_budget"]; supplied {
		if raw == nil {
			return "", fmt.Errorf("CreateGoal: token_budget must be a positive integer")
		}
		n, err := IntArgChecked(ctx.Args, "token_budget", 0)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("CreateGoal: token_budget must be a positive integer")
		}
		value := int64(n)
		budget = &value
	}
	goal, err := controller.CreateGoal(objective, budget)
	return goalResult(goal, err)
}

func (*GetGoalTool) Run(ctx *Context) (string, error) {
	controller, err := goalController(ctx)
	if err != nil {
		return "", err
	}
	goal, err := controller.GetGoal()
	return goalResult(goal, err)
}

func (*UpdateGoalTool) Run(ctx *Context) (string, error) {
	controller, err := goalController(ctx, "status", "reason")
	if err != nil {
		return "", err
	}
	status := StringArg(ctx.Args, "status", "")
	if status != "complete" && status != "blocked" && status != "paused" {
		return "", fmt.Errorf("UpdateGoal: status must be complete, blocked or paused")
	}
	reason := strings.TrimSpace(StringArg(ctx.Args, "reason", ""))
	if reason == "" {
		return "", fmt.Errorf("UpdateGoal: reason must describe completion evidence, the blocker or the pause request")
	}
	goal, err := controller.UpdateGoal(status, reason)
	return goalResult(goal, err)
}

func goalController(ctx *Context, allowedArgs ...string) (GoalControl, error) {
	if err := ctx.checkResources(); err != nil {
		return nil, err
	}
	if ctx.Context != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if ctx.Goals == nil {
		return nil, fmt.Errorf("goals are not available in this session")
	}
	for name := range ctx.Args {
		allowed := false
		for _, candidate := range allowedArgs {
			if name == candidate {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("goal tool: unexpected argument %q", name)
		}
	}
	return ctx.Goals, nil
}

func goalResult(goal *protocol.Goal, err error) (string, error) {
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(goal)
	return string(data), err
}
