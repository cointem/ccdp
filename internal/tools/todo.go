package tools

import (
	"fmt"
	"strings"
)

// TodoState is a validated task replacement handed to the session owner.
type TodoState struct {
	ID       string
	Content  string
	Status   string // pending | in_progress | completed
	Priority string // high | medium | low
}

// TodoSection renders the committed task list for system-prompt injection.
func TodoSection(states []TodoState) string {
	if len(states) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# Task list\n")
	for _, st := range states {
		priority := st.Priority
		if priority == "" {
			// Older journal records had no priority.
			priority = "medium"
		}
		fmt.Fprintf(&sb, "- %s (%s) %s\n", todoMark(st.Status), priority, st.Content)
	}
	return sb.String()
}

func todoMark(status string) string {
	switch status {
	case "in_progress":
		return "[~]"
	case "completed":
		return "[x]"
	default:
		return "[ ]"
	}
}

// TodoWriteTool maintains a working task list for the current session.
type TodoWriteTool struct{}

// NewTodoWriteTool creates the todo tool.
func NewTodoWriteTool() *TodoWriteTool { return &TodoWriteTool{} }

func (t *TodoWriteTool) Name() string { return "TodoWrite" }

func (t *TodoWriteTool) Description() string {
	return `Maintain a task list for the current goal. Use this to break a request into
steps, mark progress, and keep the plan visible. Each item has content, status
and priority. Replaces the whole list when called. The list is shown to you in
the system prompt every turn and persists for the session.`
}

func (t *TodoWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "The full list of tasks, each with content, status (pending/in_progress/completed) and priority (high/medium/low).",
				"items": map[string]any{
					"type": "object", "required": []string{"content"}, "additionalProperties": false,
					"properties": map[string]any{
						"content":  map[string]any{"type": "string"},
						"status":   map[string]any{"default": "pending", "type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						"priority": map[string]any{"default": "medium", "type": "string", "enum": []string{"high", "medium", "low"}},
					},
				},
			},
		},
		"required": []string{"todos"},
	}
}

func (t *TodoWriteTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
		if err := ctx.checkResources(); err != nil {
			return "", err
		}
		raw, ok := ctx.Args["todos"].([]any)
		if !ok {
			return "", fmt.Errorf("TodoWrite: todos must be a list")
		}
		if ctx.ReplaceTodos == nil {
			return "", fmt.Errorf("TodoWrite: session task writer is unavailable")
		}
		var states []TodoState
		for i, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("TodoWrite: item %d must be an object", i+1)
			}
			st := TodoState{
				ID:       fmt.Sprintf("%d", i+1),
				Content:  StringArg(m, "content", ""),
				Status:   StringArg(m, "status", "pending"),
				Priority: StringArg(m, "priority", "medium"),
			}
			if strings.TrimSpace(st.Content) == "" {
				return "", fmt.Errorf("TodoWrite: item %d has empty content", i+1)
			}
			if st.Status != "pending" && st.Status != "in_progress" && st.Status != "completed" {
				return "", fmt.Errorf("TodoWrite: invalid status in item %d", i+1)
			}
			if st.Priority != "high" && st.Priority != "medium" && st.Priority != "low" {
				return "", fmt.Errorf("TodoWrite: invalid priority in item %d", i+1)
			}
			states = append(states, st)
		}
		if err := ctx.ReplaceTodos(states); err != nil {
			return "", err
		}

		var sb strings.Builder
		sb.WriteString("Task list updated:\n")
		for _, st := range states {
			fmt.Fprintf(&sb, "  %s %s (%s) %s\n", todoMark(st.Status), st.ID, st.Priority, st.Content)
		}
		return sb.String(), nil
	})
}
