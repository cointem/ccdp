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

// TodoState tracks a task item.
type TodoState struct {
	ID       string
	Content  string
	Status   string // pending | in_progress | completed
	Priority string // high | medium | low
}

// TodoStore owns one session's task list. It intentionally does not use a
// package-level cache: two sessions may use the same process concurrently and
// must never observe or overwrite each other's in-memory state.
type TodoStore struct {
	owner      string
	sessionDir string

	mu     sync.Mutex
	state  map[string]TodoState
	loaded bool
	closed bool
}

// NewTodoStore creates a task-list store. The owner is only an identity used
// in diagnostics; sessionDir is the durable directory for todos.json.
func NewTodoStore(owner, sessionDir string) *TodoStore {
	return &TodoStore{owner: owner, sessionDir: sessionDir, state: map[string]TodoState{}}
}

// Owner returns the immutable scope identifier.
func (s *TodoStore) Owner() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// SessionDir returns the durable directory used by this store.
func (s *TodoStore) SessionDir() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionDir
}

func (s *TodoStore) checkOpenLocked() error {
	if s == nil {
		return fmt.Errorf("todo: nil store")
	}
	if s.closed {
		return fmt.Errorf("todo: store for %q is closed", s.owner)
	}
	return nil
}

// CheckOpen verifies that this owner can still accept a tool update.
func (s *TodoStore) CheckOpen() error {
	if s == nil {
		return fmt.Errorf("todo: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkOpenLocked()
}

// Close makes the store unavailable to subsequent tool calls. Durable data is
// left intact for the next explicit session open.
func (s *TodoStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *TodoStore) loadLocked() map[string]TodoState {
	if s.state == nil {
		s.state = map[string]TodoState{}
	}
	if s.loaded {
		return s.state
	}
	s.loaded = true
	if s.sessionDir == "" {
		return s.state
	}
	data, err := os.ReadFile(filepath.Join(s.sessionDir, "todos.json"))
	if err == nil {
		// Decode into a temporary map so malformed data cannot partially replace
		// the current in-memory list.
		var loaded map[string]TodoState
		if json.Unmarshal(data, &loaded) == nil && loaded != nil {
			s.state = loaded
		}
	}
	return s.state
}

func cloneTodoMap(in map[string]TodoState) map[string]TodoState {
	out := make(map[string]TodoState, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Load returns an independent copy of the current task map.
func (s *TodoStore) Load() (map[string]TodoState, error) {
	if s == nil {
		return nil, fmt.Errorf("todo: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	return cloneTodoMap(s.loadLocked()), nil
}

// Store replaces the task map and durably persists it before returning. The
// caller retains no mutable alias to the stored map.
func (s *TodoStore) Store(m map[string]TodoState) error {
	if s == nil {
		return fmt.Errorf("todo: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	s.state = cloneTodoMap(m)
	s.loaded = true
	if s.sessionDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.sessionDir, 0o755); err != nil {
		return fmt.Errorf("todo: create session directory: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("todo: encode state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.sessionDir, "todos.json"), data, 0o600); err != nil {
		return fmt.Errorf("todo: persist state: %w", err)
	}
	return nil
}

// Section renders the current task list for system-prompt injection. Empty
// when there are no todos (so the prompt stays stable and cheap).
func (s *TodoStore) Section() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkOpenLocked() != nil {
		return ""
	}
	m := s.loadLocked()
	if len(m) == 0 {
		return ""
	}
	states := sortedStates(m)
	var sb strings.Builder
	sb.WriteString("# Task list\n")
	for _, st := range states {
		fmt.Fprintf(&sb, "- %s (%s) %s\n", todoMark(st.Status), st.Priority, st.Content)
	}
	return sb.String()
}

// Snapshot returns a copy of the current task list for rendering.
func (s *TodoStore) Snapshot() []TodoState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkOpenLocked() != nil {
		return nil
	}
	return sortedStates(s.loadLocked())
}

func sortedStates(m map[string]TodoState) []TodoState {
	out := make([]TodoState, 0, len(m))
	for _, st := range m {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	raw, ok := ctx.Args["todos"].([]any)
	if !ok {
		return "", fmt.Errorf("TodoWrite: todos must be a list")
	}
	store, err := ctx.todoStoreForUse()
	if err != nil {
		return "", err
	}
	if err := store.CheckOpen(); err != nil {
		return "", err
	}
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
	if err := store.Store(newState); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("Task list updated:\n")
	for _, st := range states {
		fmt.Fprintf(&sb, "  %s %s (%s) %s\n", todoMark(st.Status), st.ID, st.Priority, st.Content)
	}
	return sb.String(), nil
}
