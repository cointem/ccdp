package tools

import (
	"fmt"
	"strings"
)

// TaskTool launches independent sub-agents to complete delegated subtasks
// (Claude Code's Task tool). With the "agents" parameter a batch of sub-agents
// runs concurrently, each with its own system prompt and history while sharing
// the parent's tools, sandbox and permissions. Results come back in input order.
type TaskTool struct{}

// NewTaskTool creates the Task tool.
func NewTaskTool() *TaskTool { return &TaskTool{} }

func (t *TaskTool) Name() string { return "Task" }

func (t *TaskTool) Description() string {
	return `Delegate self-contained subtasks to independent sub-agents that run their own
agent loops with your tools, sandbox and permissions. Use this to parallelize
work: e.g. research, draft, or verify pieces of a large task while you continue
the main thread.

- For a single subtask pass "description".
- To fan out N independent pieces of work in parallel, pass "agents": an array
  of {"description": ..., "system_prompt": ...} objects. All agents run
  concurrently and their final answers are returned in order.
Keep each delegation focused and self-contained. Sub-agents cannot ask the user
anything; they work within the permissions they inherit.`
}

func (t *TaskTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
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
	if ctx.Subagent == nil && ctx.Subagents == nil {
		return "", fmt.Errorf("Task: sub-agents are not available in this session")
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
				Description:  desc,
				SystemPrompt: StringArg(obj, "system_prompt", ""),
			})
		}
		results, err := ctx.Subagents(tasks)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, r := range results {
			fmt.Fprintf(&sb, "── sub-agent %d: %s ──\n", r.Index+1, r.Description)
			if r.Error != "" {
				fmt.Fprintf(&sb, "ERROR: %s\n", r.Error)
			} else {
				sb.WriteString(strings.TrimSpace(r.Output))
				sb.WriteString("\n")
			}
		}
		return strings.TrimRight(sb.String(), "\n"), nil
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
	return ctx.Subagent(description, system)
}
