package agent

import (
	"fmt"

	"ccdp/internal/tools"
)

// EnterPlanModeTool lets the model proactively enter plan mode (Claude Code's
// EnterPlanMode tool): the next model calls are restricted to read-only tools
// and it must produce a plan before the user approves execution.
type enterPlanModeTool struct{ ag *Agent }

func (t *enterPlanModeTool) Name() string { return "EnterPlanMode" }

func (t *enterPlanModeTool) Description() string {
	return `Enter plan mode: stop executing and switch to producing a plan first. While
plan mode is active use read-only tools to investigate and AskUserQuestion to
clarify requirements. Finish by
calling ExitPlanMode with the proposed plan so the user can approve it before
any changes are made. Call this when the user asks for a plan, or when the task
is complex and you should propose an approach before executing.`
}

func (t *enterPlanModeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *enterPlanModeTool) Run(ctx *tools.Context) (string, error) {
	if err := t.ag.setPlanMode(true); err != nil {
		return "", err
	}
	t.ag.emitStatus("entered plan mode (via EnterPlanMode)")
	return `Plan mode is now active. Investigate with read-only tools, then call
ExitPlanMode with your proposed plan for approval.`, nil
}

// ExitPlanModeTool submits a plan for approval (Claude Code's ExitPlanMode
// tool). It blocks until the user approves or rejects; on approval plan mode
// exits and the model may execute the plan.
type exitPlanModeTool struct{ ag *Agent }

func (t *exitPlanModeTool) Name() string { return "ExitPlanMode" }

func (t *exitPlanModeTool) Description() string {
	return `Submit your proposed plan for the user's approval. Provide the full plan as
the "plan" argument. Execution is paused until the user decides: if approved,
plan mode exits and you may execute the plan; if rejected, stay in plan mode
and adjust the plan based on the rejection.`
}

func (t *exitPlanModeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "The complete execution plan, step by step, with exact files and commands.",
			},
		},
		"required": []string{"plan"},
	}
}

func (t *exitPlanModeTool) Run(ctx *tools.Context) (string, error) {
	plan := tools.StringArg(ctx.Args, "plan", "")
	if plan == "" {
		return "", fmt.Errorf("ExitPlanMode: plan is required")
	}
	t.ag.emitStatus("submitted plan for approval…")
	if t.ag.requestPlanApproval(plan) {
		if err := t.ag.setPlanMode(false); err != nil {
			return "", err
		}
		return "Plan approved. Plan mode is off — execute the plan now, step by step, and verify each step.", nil
	}
	return "Plan not approved. Remain in plan mode: revise the plan based on the feedback, then call ExitPlanMode again.", nil
}
