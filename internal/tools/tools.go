// Package tools defines the tool registry and every built-in tool the agent
// can invoke. Each tool declares a JSON Schema for its arguments and a Run
// method. Permission decisions are made by the permissions package, not here.
package tools

import (
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SubagentFunc runs an independent sub-agent: a full mini agent loop with its
// own system prompt and history, sharing the parent's tools, sandbox and
// permissions. It returns the sub-agent's final text output.
type SubagentFunc func(description, systemPrompt string) (string, error)

// SubagentTask is one unit of work in a batched Task invocation.
type SubagentTask struct {
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

// SubagentResult is one batched task's outcome. Error is "" on success.
type SubagentResult struct {
	Index       int    `json:"index"`
	Description string `json:"description"`
	Output      string `json:"output"`
	Error       string `json:"error,omitempty"`
}

// SubagentsFunc runs a batch of independent sub-agents concurrently, returning
// one result per task in input order (Claude Code's parallel Task agents).
type SubagentsFunc func(tasks []SubagentTask) ([]SubagentResult, error)

// Context carries per-invocation state into a tool.
type Context struct {
	context.Context
	WorkingDir string         // directory the tool should operate in
	SessionDir string         // directory where the session persists (todos, etc.)
	Args       map[string]any // parsed JSON arguments from the model
	Timeout    time.Duration  // per-tool timeout (0 = default)
	// Sandbox confines path resolution to the workspace. When nil, paths are
	// resolved against WorkingDir without containment.
	Sandbox *sandbox.Sandbox
	// Notify is called with transient progress updates (e.g. command output)
	// that the UI can stream live for long-running tools.
	Notify func(line string)
	// Subagent launches an independent sub-agent loop (Claude Code's Task tool
	// idea). Nil means sub-agents are unavailable in this session.
	Subagent SubagentFunc
	// Subagents runs a batch of sub-agents concurrently (parallel Task agents).
	// Nil falls back to Subagent when only one task is requested.
	Subagents SubagentsFunc
	// Skills exposes the loaded skill store for ReadSkill lookups.
	Skills skills.Provider
}

// ResolveRead resolves p for reading, honoring the sandbox when present.
func (c *Context) ResolveRead(p string) (string, error) {
	if c.Sandbox != nil {
		return c.Sandbox.ResolveRead(p)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(c.WorkingDir, p), nil
}

// ResolveWrite resolves p for writing, honoring the sandbox when present.
func (c *Context) ResolveWrite(p string) (string, error) {
	if c.Sandbox != nil {
		return c.Sandbox.ResolveWrite(p)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(c.WorkingDir, p), nil
}

// Tool is the interface implemented by every built-in tool.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON Schema object for the tool's arguments.
	Parameters() map[string]any
	// Run executes the tool and returns its textual result.
	Run(ctx *Context) (string, error)
}

// Registry maps tool names to implementations. Tools live in named scoped
// layers (the deepseek-harness ScopedLayers idea): the built-in set lives in
// the "builtin" scope, plugins can register their own scopes, and the visible
// toolset is the merged union of all layers. Layer ordering is deterministic
// (registration order); later layers override earlier ones on name collision.
// Registering returns a disposer so callers can remove exactly what they added.
type Registry struct {
	mu        sync.RWMutex
	scopes    []string
	layers    map[string]map[string]Tool
	onChanged func()
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{layers: map[string]map[string]Tool{}}
}

// Register adds a tool to the default "global" scope. Returns a disposer.
func (r *Registry) Register(t Tool) func() {
	return r.RegisterIn("global", t)
}

// RegisterIn adds a tool to a named scope, creating the scope if needed.
// Returns a disposer that removes the tool again.
func (r *Registry) RegisterIn(scope string, t Tool) func() {
	r.mu.Lock()
	if _, ok := r.layers[scope]; !ok {
		r.scopes = append(r.scopes, scope)
		r.layers[scope] = map[string]Tool{}
	}
	r.layers[scope][t.Name()] = t
	r.mu.Unlock()
	r.changed()
	return func() {
		r.Unregister(scope, t.Name())
	}
}

// Unregister removes a tool from a scope.
func (r *Registry) Unregister(scope, name string) {
	r.mu.Lock()
	if layer, ok := r.layers[scope]; ok {
		if _, existed := layer[name]; existed {
			delete(layer, name)
			r.mu.Unlock()
			r.changed()
			return
		}
	}
	r.mu.Unlock()
}

// UnregisterScope removes an entire scope and all its tools.
func (r *Registry) UnregisterScope(scope string) {
	r.mu.Lock()
	if _, ok := r.layers[scope]; !ok {
		r.mu.Unlock()
		return
	}
	delete(r.layers, scope)
	for i, s := range r.scopes {
		if s == scope {
			r.scopes = append(r.scopes[:i], r.scopes[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	r.changed()
}

// SetChangeListener registers a callback invoked whenever the visible toolset
// changes (used by the plugin host to emit tools/change events).
func (r *Registry) SetChangeListener(fn func()) {
	r.mu.Lock()
	r.onChanged = fn
	r.mu.Unlock()
}

func (r *Registry) changed() {
	r.mu.RLock()
	fn := r.onChanged
	r.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Get returns a tool by name (later scopes take precedence).
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.scopes) - 1; i >= 0; i-- {
		if t, ok := r.layers[r.scopes[i]][name]; ok {
			return t, true
		}
	}
	return nil, false
}

// Names returns all visible tool names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	seen := map[string]bool{}
	for _, scope := range r.scopes {
		for n := range r.layers[scope] {
			seen[n] = true
		}
	}
	r.mu.RUnlock()
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schemas returns the merged tool declarations for the LLM API, sorted by name.
func (r *Registry) Schemas() []map[string]any {
	return r.SchemasFiltered(nil)
}

// SchemasFiltered returns tool declarations for names passing keep (nil keeps
// all), sorted by name. Used for deferred tool injection. Names and tools are
// collected under a single read lock so a concurrent Unregister can't produce
// a nil tool between Names() and Get().
func (r *Registry) SchemasFiltered(keep func(name string) bool) []map[string]any {
	r.mu.RLock()
	tools := make(map[string]Tool, 16)
	for _, scope := range r.scopes {
		for n, t := range r.layers[scope] {
			tools[n] = t // later scopes override earlier ones, matching Get
		}
	}
	r.mu.RUnlock()

	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)

	schemas := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if keep != nil && !keep(name) {
			continue
		}
		t := tools[name]
		if t == nil {
			continue
		}
		schemas = append(schemas, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"parameters":  t.Parameters(),
			},
		})
	}
	return schemas
}

// StringArg extracts a string argument with a fallback.
func StringArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

// IntArg extracts an int argument with a fallback (float64 from JSON is tolerated).
func IntArg(args map[string]any, key string, fallback int) int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return fallback
}

// BoolArg extracts a boolean argument with a fallback (bool from JSON).
func BoolArg(args map[string]any, key string, fallback bool) bool {
	if v, ok := args[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

// DumpArgs is a debug helper for tool authors.
func DumpArgs(args map[string]any) string {
	b, _ := json.Marshal(args)
	return string(b)
}

// WrapError formats a tool error in a stable, parseable way.
func WrapError(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}
