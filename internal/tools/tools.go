// Package tools defines the tool registry and every built-in tool the agent
// can invoke. Each tool declares a JSON Schema for its arguments and a Run
// method. Permission decisions are made by the permissions package, not here.
package tools

import (
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"time"
)

// SubagentFunc runs an independent sub-agent: a full mini agent loop with its
// own system prompt and history, sharing the parent's tools, sandbox and
// permissions. It returns the sub-agent's final text output.
type SubagentFunc func(description, systemPrompt string) (string, error)

// SubagentTask is one unit of work in a batched Task invocation.
type SubagentTask struct {
	WaitPolicy   string `json:"wait_policy,omitempty"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

// SubagentResult is one batched task's outcome. Error is "" on success.
type SubagentResult struct {
	SessionID   string `json:"session_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
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
	// Resources is the session-owned resource container. Runtime code must set
	// this field for stateful tools; narrow capability fields below are only for
	// explicit adapters that own the corresponding resource themselves.
	Resources *Resources
	// ProcessManager is an optional narrow injection for callers that only
	// provide process capabilities.  When Resources is non-nil its manager
	// takes precedence and this field is ignored.
	ProcessManager *ProcessManager
	// FileState and TodoStore are optional narrow injections.  They are useful
	// to adapters migrating one tool at a time; Resources remains preferred.
	FileState *FileState
	TodoStore *TodoStore
	// Owner is the resource scope expected by this invocation.  A non-empty
	// value is checked against Resources.Owner so a stale/cross-session context
	// cannot accidentally use another session's handles.
	Owner string
	// ReadLimit and OutputLimit bound bytes admitted from the filesystem and
	// bytes returned/retained by a tool. Zero uses package defaults.
	ReadLimit   int
	OutputLimit int
	// ProcessInputLimit bounds one ProcessWrite call. Zero uses the default.
	ProcessInputLimit int
	// Sandbox applies the shared file policy: host reads are allowed except
	// explicit denies, while writes require an authorized root.
	Sandbox *sandbox.Sandbox
	// HostNetworkAllowed authorizes this invocation's in-process network
	// adapters. Seatbelt does not constrain the harness process itself.
	HostNetworkAllowed bool
	// Notify is called with transient progress updates (e.g. command output)
	// that the UI can stream live for long-running tools.
	Notify func(line string)
	// Subagent launches an independent sub-agent loop (Claude Code's Task tool
	// idea). Nil means sub-agents are unavailable in this session.
	Subagent SubagentFunc
	// Subagents runs a batch of sub-agents concurrently (parallel Task agents).
	// Nil falls back to Subagent when only one task is requested.
	Subagents      SubagentsFunc
	Sessions       protocol.SessionDirectory
	AgentCommandID protocol.CommandID
	// Skills exposes the loaded skill store for ReadSkill lookups.
	Skills skills.Provider
}

const (
	// DefaultReadLimit is the maximum bytes one file read admits by default.
	DefaultReadLimit = 512 * 1024
	// DefaultOutputLimit bounds model-visible tool output. Raw process output is
	// bounded independently by ProcessManager.
	DefaultOutputLimit = 512 * 1024
	// DefaultProcessInputLimit bounds one write sent to a background process.
	DefaultProcessInputLimit = 64 * 1024
)

func (c *Context) readLimit() int {
	if c != nil && c.ReadLimit > 0 {
		return c.ReadLimit
	}
	return DefaultReadLimit
}

func (c *Context) outputLimit() int {
	if c != nil && c.OutputLimit > 0 {
		return c.OutputLimit
	}
	return DefaultOutputLimit
}

func (c *Context) processInputLimit() int {
	if c != nil && c.ProcessInputLimit > 0 {
		return c.ProcessInputLimit
	}
	return DefaultProcessInputLimit
}

// notifyContext adapts the runtime's short, synchronous event callback to the
// execution package's cancellable observer contract. Runtime callbacks should
// publish to their bounded event bus and return promptly; callers that may
// block should provide execution.Request.Progress instead.
func (c *Context) notifyContext() func(context.Context, string) error {
	if c == nil || c.Notify == nil {
		return nil
	}
	return func(ctx context.Context, line string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c.Notify(line)
		return nil
	}
}

// checkResources verifies that the invocation is still attached to its
// session owner. Runtime code can call this before dispatching a tool; the
// built-in tools also call their specific owner accessors.
func (c *Context) checkResources() error {
	if c == nil {
		return fmt.Errorf("tools: nil context")
	}
	if c.Resources == nil {
		return nil
	}
	if err := c.Resources.CheckOpen(); err != nil {
		return err
	}
	if c.Owner != "" && c.Owner != c.Resources.Owner() {
		return fmt.Errorf("tools: context owner %q does not match resource owner %q", c.Owner, c.Resources.Owner())
	}
	return nil
}

func (c *Context) todoStore() *TodoStore {
	if c != nil && c.Resources != nil {
		return c.Resources.Todos
	}
	if c != nil && c.TodoStore != nil {
		return c.TodoStore
	}
	return nil
}

func (c *Context) todoStoreForUse() (*TodoStore, error) {
	store := c.todoStore()
	if store == nil {
		return nil, fmt.Errorf("tools: no session TodoStore injected")
	}
	if err := store.CheckOpen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (c *Context) fileState() *FileState {
	if c != nil && c.Resources != nil {
		return c.Resources.Files
	}
	if c != nil && c.FileState != nil {
		return c.FileState
	}
	return nil
}

func (c *Context) fileStateForUse() (*FileState, error) {
	state := c.fileState()
	if state == nil {
		return nil, fmt.Errorf("tools: no session FileState injected")
	}
	if err := state.CheckOpen(); err != nil {
		return nil, err
	}
	return state, nil
}

func (c *Context) processManager() (*ProcessManager, error) {
	if err := c.checkResources(); err != nil {
		return nil, err
	}
	if c.Resources != nil {
		if c.Resources.Processes == nil {
			return nil, fmt.Errorf("tools: resource owner %q has no process manager", c.Resources.Owner())
		}
		return c.Resources.Processes, nil
	}
	if c.ProcessManager != nil {
		if err := c.ProcessManager.CheckOpen(); err != nil {
			return nil, err
		}
		return c.ProcessManager, nil
	}
	return nil, fmt.Errorf("tools: no session ProcessManager injected")
}

// ResolveRead resolves p for reading. A missing policy is a denied operation.
func (c *Context) ResolveRead(p string) (string, error) {
	if err := c.checkResources(); err != nil {
		return "", err
	}
	if c.Sandbox == nil {
		return "", fmt.Errorf("tools: sandbox policy is unavailable")
	}
	return c.Sandbox.ResolveRead(p)
}

// ResolveWrite resolves p for writing. A missing policy is a denied operation.
func (c *Context) ResolveWrite(p string) (string, error) {
	if err := c.checkResources(); err != nil {
		return "", err
	}
	if c.Sandbox == nil {
		return "", fmt.Errorf("tools: sandbox policy is unavailable")
	}
	return c.Sandbox.ResolveWrite(p)
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
	layers    map[string]map[string]registryEntry
	nextToken uint64
	version   uint64
	onChanged func()
}

type registryEntry struct {
	tool  Tool
	token uint64
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{layers: map[string]map[string]registryEntry{}}
}

// Register adds a tool to the default "global" scope. Returns a disposer.
func (r *Registry) Register(t Tool) func() {
	return r.RegisterIn("global", t)
}

// RegisterIn adds a tool to a named scope, creating the scope if needed.
// Returns a disposer that removes the tool again.
func (r *Registry) RegisterIn(scope string, t Tool) func() {
	if r == nil || t == nil || scope == "" || t.Name() == "" {
		return func() {}
	}
	r.mu.Lock()
	if _, ok := r.layers[scope]; !ok {
		r.scopes = append(r.scopes, scope)
		r.layers[scope] = map[string]registryEntry{}
	}
	r.nextToken++
	token := r.nextToken
	r.layers[scope][t.Name()] = registryEntry{tool: t, token: token}
	r.version++
	r.mu.Unlock()
	r.changed()
	return func() {
		r.unregisterToken(scope, t.Name(), token)
	}
}

func (r *Registry) unregisterToken(scope, name string, token uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	layer, ok := r.layers[scope]
	entry, present := layer[name]
	if !ok || !present || entry.token != token {
		r.mu.Unlock()
		return
	}
	delete(layer, name)
	r.version++
	r.mu.Unlock()
	r.changed()
}

// Unregister removes a tool from a scope.
func (r *Registry) Unregister(scope, name string) {
	r.mu.Lock()
	if layer, ok := r.layers[scope]; ok {
		if _, existed := layer[name]; existed {
			delete(layer, name)
			r.version++
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
	r.version++
	for i, s := range r.scopes {
		if s == scope {
			r.scopes = append(r.scopes[:i], r.scopes[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	r.changed()
}

// ReplaceScope publishes a complete scope in one registry critical section.
// Readers using Acquire observe either the previous directory or the new one,
// never an empty/half-registered intermediate view. Each replacement gets new
// tokens, so disposers from an older generation cannot remove these entries.
func (r *Registry) ReplaceScope(scope string, values []Tool) {
	if r == nil || scope == "" {
		return
	}
	r.mu.Lock()
	if r.layers == nil {
		r.layers = map[string]map[string]registryEntry{}
	}
	if _, ok := r.layers[scope]; !ok {
		r.scopes = append(r.scopes, scope)
	}
	layer := make(map[string]registryEntry, len(values))
	for _, tool := range values {
		if tool == nil || tool.Name() == "" {
			continue
		}
		r.nextToken++
		layer[tool.Name()] = registryEntry{tool: tool, token: r.nextToken}
	}
	r.layers[scope] = layer
	r.version++
	r.mu.Unlock()
	r.changed()
}

// ReplaceScopes publishes several project-owned scopes as one visible
// registry generation. Existing scope order is preserved (later scopes still
// override earlier ones); newly introduced scopes are appended in stable name
// order. This is used by settings reload so observers cannot see custom tools
// from one candidate mixed with web tools from another.
func (r *Registry) ReplaceScopes(values map[string][]Tool) {
	if r == nil || len(values) == 0 {
		return
	}
	r.mu.Lock()
	if r.layers == nil {
		r.layers = map[string]map[string]registryEntry{}
	}
	newScopes := make([]string, 0, len(values))
	for scope := range values {
		if scope == "" {
			continue
		}
		if _, exists := r.layers[scope]; !exists {
			newScopes = append(newScopes, scope)
		}
	}
	sort.Strings(newScopes)
	r.scopes = append(r.scopes, newScopes...)
	for scope, tools := range values {
		if scope == "" {
			continue
		}
		layer := make(map[string]registryEntry, len(tools))
		for _, tool := range tools {
			if tool == nil || tool.Name() == "" {
				continue
			}
			r.nextToken++
			layer[tool.Name()] = registryEntry{tool: tool, token: r.nextToken}
		}
		r.layers[scope] = layer
	}
	r.version++
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
		if entry, ok := r.layers[r.scopes[i]][name]; ok {
			return entry.tool, true
		}
	}
	return nil, false
}

// Version changes whenever a visible registry binding may change. A step can
// retain its Lease even while this value advances; the version is diagnostic
// and must never be used to look up the live implementation later.
func (r *Registry) Version() uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	v := r.version
	r.mu.RUnlock()
	return v
}

// Lease is an immutable view of the visible registry at one step boundary.
// It owns no registry lock: implementations remain reachable until the lease
// is discarded, so unregister/replace cannot ABA a model tool call into a new
// implementation during a stream. Close is present for symmetry and future
// ref-counted implementations; current Tool values are Go-owned objects.
type Lease struct {
	version uint64
	tools   map[string]Tool
}

// Acquire returns a frozen visible binding. The returned map is private and
// never aliases registry layers or caller-owned maps.
func (r *Registry) Acquire() *Lease {
	if r == nil {
		return &Lease{tools: map[string]Tool{}}
	}
	r.mu.RLock()
	view := make(map[string]Tool)
	for _, scope := range r.scopes {
		for name, entry := range r.layers[scope] {
			view[name] = entry.tool
		}
	}
	version := r.version
	r.mu.RUnlock()
	return &Lease{version: version, tools: view}
}

// Snapshot is a descriptive alias for Acquire used by step/runtime code.
func (r *Registry) Snapshot() *Lease { return r.Acquire() }

func (l *Lease) Version() uint64 {
	if l == nil {
		return 0
	}
	return l.version
}

func (l *Lease) Get(name string) (Tool, bool) {
	if l == nil {
		return nil, false
	}
	t, ok := l.tools[name]
	return t, ok
}

func (l *Lease) Names() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.tools))
	for name := range l.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (l *Lease) SchemasFiltered(keep func(name string) bool) []map[string]any {
	if l == nil {
		return nil
	}
	names := l.Names()
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if keep != nil && !keep(name) {
			continue
		}
		t := l.tools[name]
		if t == nil {
			continue
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name(), "description": t.Description(), "parameters": t.Parameters(),
		}})
	}
	return out
}

func (l *Lease) Close() {}

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
		for n, entry := range r.layers[scope] {
			tools[n] = entry.tool // later scopes override earlier ones, matching Get
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

// IntArg extracts an int argument with a fallback. New runtime/tool code
// should prefer IntArgChecked so malformed, fractional, or overflowing JSON
// numbers cannot silently turn into an unrelated offset, pid, or timeout.
func IntArg(args map[string]any, key string, fallback int) int {
	n, err := IntArgChecked(args, key, fallback)
	if err != nil {
		return fallback
	}
	return n
}

// IntArgChecked parses the integer forms produced by both the legacy JSON
// decoder (float64) and the strict UseNumber decoder (json.Number). It rejects
// fractional, non-finite, and out-of-range values instead of truncating them.
func IntArgChecked(args map[string]any, key string, fallback int) (int, error) {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return fallback, fmt.Errorf("tools: %s must be a finite integer", key)
			}
			return checkedIntegerString(key, strconv.FormatFloat(n, 'g', -1, 64), fallback)
		case float32:
			if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
				return fallback, fmt.Errorf("tools: %s must be a finite integer", key)
			}
			return checkedIntegerString(key, strconv.FormatFloat(float64(n), 'g', -1, 32), fallback)
		case int:
			return n, nil
		case int8:
			return int(n), nil
		case int16:
			return int(n), nil
		case int32:
			return int(n), nil
		case int64:
			return checkedIntegerString(key, strconv.FormatInt(n, 10), fallback)
		case uint:
			return checkedIntegerString(key, strconv.FormatUint(uint64(n), 10), fallback)
		case uint8:
			return int(n), nil
		case uint16:
			return int(n), nil
		case uint32:
			return checkedIntegerString(key, strconv.FormatUint(uint64(n), 10), fallback)
		case uint64:
			return checkedIntegerString(key, strconv.FormatUint(n, 10), fallback)
		case json.Number:
			return checkedIntegerString(key, string(n), fallback)
		default:
			return fallback, fmt.Errorf("tools: %s must be an integer", key)
		}
	}
	return fallback, nil
}

func checkedIntegerString(key, value string, fallback int) (int, error) {
	rational, ok := new(big.Rat).SetString(value)
	if !ok || !rational.IsInt() {
		return fallback, fmt.Errorf("tools: %s must be an integer", key)
	}
	n := rational.Num()
	if !n.IsInt64() {
		return fallback, fmt.Errorf("tools: %s is outside int range", key)
	}
	value64 := n.Int64()
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	if value64 < minInt || value64 > maxInt {
		return fallback, fmt.Errorf("tools: %s is outside int range", key)
	}
	return int(value64), nil
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

// boundedToolString enforces the output admission limit at the final tool
// boundary, including status text appended after subprocess output.
func boundedToolString(ctx *Context, value string) string {
	limit := DefaultOutputLimit
	if ctx != nil && ctx.OutputLimit > 0 {
		limit = ctx.OutputLimit
	}
	if limit <= 0 || len(value) <= limit {
		return value
	}
	marker := "\n…[tool output truncated]"
	if limit <= len(marker) {
		return value[:limit]
	}
	return value[:limit-len(marker)] + marker
}
