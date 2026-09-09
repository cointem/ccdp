package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// todoStore keeps per-session task lists (keyed by session dir) and persists
// them to <session dir>/todos.json so the model's task list survives restarts
// (Claude Code's scratchpad / TodoWrite idea).
var todoStore = &todoMemory{bySession: map[string]map[string]TodoState{}}

type todoMemory struct {
	mu        sync.Mutex
	bySession map[string]map[string]TodoState
}

// TodoState tracks a task item.
type TodoState struct {
	ID       string
	Content  string
	Status   string // pending | in_progress | completed
	Priority string // high | medium | low
}

// load returns the todo map for a session, loading from disk on first access.
func (s *todoMemory) load(dir string) map[string]TodoState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.bySession[dir]; ok {
		return m
	}
	m := map[string]TodoState{}
	if data, err := os.ReadFile(filepath.Join(dir, "todos.json")); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	s.bySession[dir] = m
	return m
}

// store replaces the todo map for a session and persists it.
func (s *todoMemory) store(dir string, m map[string]TodoState) {
	s.mu.Lock()
	s.bySession[dir] = m
	s.mu.Unlock()
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		data, _ := json.MarshalIndent(m, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "todos.json"), data, 0o600)
	}
}

// Section renders the current task list for system-prompt injection. Empty
// when there are no todos (so the prompt stays stable and cheap).
func (s *todoMemory) Section(dir string) string {
	m := s.load(dir)
	if len(m) == 0 {
		return ""
	}
	states := sortedStates(m)
	var sb strings.Builder
	sb.WriteString("# Task list\n")
	for _, st := range states {
		mark := "[ ]"
		switch st.Status {
		case "in_progress":
			mark = "[~]"
		case "completed":
			mark = "[x]"
		}
		fmt.Fprintf(&sb, "- %s (%s) %s\n", mark, st.Priority, st.Content)
	}
	return sb.String()
}

// TodoSection renders the session's task list for system-prompt injection
// ("" when empty).
func TodoSection(sessionDir string) string {
	return todoStore.Section(sessionDir)
}

// Snapshot returns a copy of the current todo list for rendering.
func Snapshot(dir string) []TodoState {
	return sortedStates(todoStore.load(dir))
}

func sortedStates(m map[string]TodoState) []TodoState {
	out := make([]TodoState, 0, len(m))
	for _, st := range m {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
					"type": "object",
					"properties": map[string]any{
						"content":  map[string]any{"type": "string"},
						"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
					},
				},
			},
		},
		"required": []string{"todos"},
	}
}

func (t *TodoWriteTool) Run(ctx *Context) (string, error) {
	raw, ok := ctx.Args["todos"].([]any)
	if !ok {
		return "", fmt.Errorf("TodoWrite: todos must be a list")
	}
	dir := ctx.SessionDir
	newState := map[string]TodoState{}
	var states []TodoState
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		st := TodoState{
			ID:       fmt.Sprintf("%d", i+1),
			Content:  StringArg(m, "content", ""),
			Status:   StringArg(m, "status", "pending"),
			Priority: StringArg(m, "priority", "medium"),
		}
		if st.Content == "" {
			continue
		}
		newState[st.ID] = st
		states = append(states, st)
	}
	todoStore.store(dir, newState)

	var sb strings.Builder
	sb.WriteString("Task list updated:\n")
	for _, st := range states {
		mark := "[ ]"
		switch st.Status {
		case "in_progress":
			mark = "[~]"
		case "completed":
			mark = "[x]"
		}
		fmt.Fprintf(&sb, "  %s %s (%s) %s\n", mark, st.ID, st.Priority, st.Content)
	}
	return sb.String(), nil
}
