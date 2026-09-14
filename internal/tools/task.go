package tools

import (
	"fmt"
	"strings"
)

// TaskTool launches independent child sessions to complete delegated subtasks
// (Claude Code's Task tool). Each child runs its own loop, history and session
// resources under a frozen snapshot of the parent's effective configuration,
// permissions and admitted tools. With the "agents" parameter, a bounded
// parent-level slot pool runs children concurrently and returns results in
// input order; child lifecycle hooks run per item. Mutable project hooks, MCP
// startup and memory are not inherited. Interactive approvals remain subject
// to inherited hard policy; nested delegation is unavailable.
type TaskTool struct{}

// NewTaskTool creates the Task tool.
func NewTaskTool() *TaskTool { return &TaskTool{} }

func (t *TaskTool) Name() string { return "Task" }

func (t *TaskTool) Description() string {
	return `Delegate self-contained subtasks to independent child sessions that run their own
agent loops with a frozen snapshot of the effective configuration, permissions,
and explicitly admitted tools. Use this to parallelize work: e.g. research,
draft, or verify pieces of a large task while you continue the main thread.

- For a single subtask pass "description".
- To fan out N independent pieces of work in parallel, pass "agents": an array
  of {"description": ..., "system_prompt": ...} objects. All agents run
  concurrently up to the parent session's child-slot limit, and their final
  answers are returned in order.
- wait_policy="join" (default) waits for completion. wait_policy="notify"
  returns session/run IDs immediately and sends a completion input later.
- Use Agent to list/read/wait/control children, including sending followups.
Keep each delegation focused and self-contained. Children cannot launch another
Task or control sibling agents. Approvals require an interactive human channel;
headless calls requiring approval are denied. Hard inherited denies always apply.`
}

func (t *TaskTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wait_policy": map[string]any{"type": "string", "enum": []string{"join", "notify"}, "description": "join waits for results (default); notify returns child/run IDs and delivers completion in a later parent input."},
			"description": map[string]any{
				"type":        "string",
				"description": "The task a single sub-agent should accomplish, in enough detail to work independently.",
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "Optional custom system prompt for the sub-agent (overrides the default).",
			},
			"agents": map[string]any{
				"type":        "array",
				"description": "Optional batch of subtasks to run concurrently. Each item has description (required) and optional system_prompt.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"description": map[string]any{
							"type":        "string",
							"description": "The subtask this agent should accomplish.",
						},
						"system_prompt": map[string]any{
							"type":        "string",
							"description": "Optional custom system prompt for this sub-agent.",
						},
					},
					"required": []string{"description"},
				},
			},
		},
		"oneOf": []any{
			map[string]any{"required": []string{"description"}},
			map[string]any{"required": []string{"agents"}},
		},
	}
}

func (t *TaskTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	if ctx.Subagent == nil && ctx.Subagents == nil {
		return "", fmt.Errorf("Task: sub-agents are not available in this session")
	}
	policy := StringArg(ctx.Args, "wait_policy", "join")
	if policy != "join" && policy != "notify" {
		return "", fmt.Errorf("Task: wait_policy must be join or notify")
	}

	// Batch mode: fan out N parallel sub-agents.
	if agents, ok := ctx.Args["agents"].([]any); ok && len(agents) > 0 {
		if ctx.Subagents == nil {
			return "", fmt.Errorf("Task: parallel sub-agents are not available in this session")
		}
		tasks := make([]SubagentTask, 0, len(agents))
		for i, a := range agents {
			obj, ok := a.(map[string]any)
			if !ok {
				return "", fmt.Errorf("Task: agents[%d] must be an object", i)
			}
			desc := strings.TrimSpace(StringArg(obj, "description", ""))
			if desc == "" {
				return "", fmt.Errorf("Task: agents[%d].description is required", i)
			}
			tasks = append(tasks, SubagentTask{
				WaitPolicy:   policy,
				Description:  desc,
				SystemPrompt: StringArg(obj, "system_prompt", ""),
			})
		}
		results, err := ctx.Subagents(tasks)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		limit := ctx.outputLimit()
		truncated := false
		appendOutput := func(value string) {
			if truncated {
				return
			}
			if limit <= 0 || len(value) <= limit-sb.Len() {
				sb.WriteString(value)
				return
			}
			marker := "\n…[tool output truncated]"
			if limit <= sb.Len() {
				truncated = true
				return
			}
			remaining := limit - sb.Len()
			if remaining <= len(marker) {
				sb.WriteString(value[:remaining])
			} else {
				sb.WriteString(value[:remaining-len(marker)])
				sb.WriteString(marker)
			}
			truncated = true
		}
		for _, r := range results {
			appendOutput(fmt.Sprintf("── sub-agent %d: %s ──\n", r.Index+1, r.Description))
			if r.SessionID != "" {
				appendOutput(fmt.Sprintf("session=%s run=%s\n", r.SessionID, r.RunID))
			}
			if r.Error != "" {
				appendOutput(fmt.Sprintf("ERROR: %s\n", r.Error))
			} else {
				appendOutput(strings.TrimSpace(r.Output))
				appendOutput("\n")
			}
		}
		return boundedToolString(ctx, strings.TrimRight(sb.String(), "\n")), nil
	}

	// Single mode.
	if ctx.Subagent == nil {
		return "", fmt.Errorf("Task: sub-agents are not available in this session")
	}
	description := strings.TrimSpace(StringArg(ctx.Args, "description", ""))
	if description == "" {
		return "", fmt.Errorf("Task: description is required")
	}
	system := StringArg(ctx.Args, "system_prompt", "")
	if policy == "notify" {
		if ctx.Subagents == nil {
			return "", fmt.Errorf("Task: background delegation unavailable")
		}
		results, err := ctx.Subagents([]SubagentTask{{Description: description, SystemPrompt: system, WaitPolicy: policy}})
		if err != nil {
			return "", err
		}
		if results[0].Error != "" {
			return "", fmt.Errorf("%s", results[0].Error)
		}
		return boundedToolString(ctx, results[0].Output), nil
	}
	out, err := ctx.Subagent(description, system)
	if err != nil {
		return "", err
	}
	return boundedToolString(ctx, out), nil
}
