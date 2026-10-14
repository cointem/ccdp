package tools

import (
	"ccdp/internal/jsonvalue"
	"encoding/json"

	"sort"

	"sync"
)

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

// Definition is frozen when its implementation is registered. Callers receive
// independent schema values; acquiring a lease never invokes extension code.
type Definition struct {
	Name, Description string
	Parameters        map[string]any
	schemaJSON        []byte
}

type registryEntry struct {
	tool       Tool
	token      uint64
	definition Definition
}

func prepareRegistryEntry(tool Tool) registryEntry {
	definition := Definition{Name: tool.Name(), Description: tool.Description(), Parameters: jsonvalue.Clone(tool.Parameters())}
	definition.schemaJSON, _ = json.Marshal(definition.Parameters)
	return registryEntry{tool: tool, definition: definition}
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
	if r == nil || t == nil || scope == "" {
		return func() {}
	}
	entry := prepareRegistryEntry(t)
	name := entry.definition.Name
	if name == "" {
		return func() {}
	}
	r.mu.Lock()
	if _, ok := r.layers[scope]; !ok {
		r.scopes = append(r.scopes, scope)
		r.layers[scope] = map[string]registryEntry{}
	}
	r.nextToken++
	entry.token = r.nextToken
	r.layers[scope][name] = entry
	r.version++
	r.mu.Unlock()
	r.changed()
	return func() { r.unregisterToken(scope, name, entry.token) }
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
	if scope == "" {
		return
	}
	r.ReplaceScopes(map[string][]Tool{scope: values})
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
	// Capture extension metadata before taking the registry publication lock.
	prepared := make(map[string]map[string]registryEntry, len(values))
	for scope, tools := range values {
		layer := make(map[string]registryEntry, len(tools))
		for _, tool := range tools {
			if tool == nil {
				continue
			}
			entry := prepareRegistryEntry(tool)
			if entry.definition.Name != "" {
				layer[entry.definition.Name] = entry
			}
		}
		prepared[scope] = layer
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
	for scope, layer := range prepared {
		if scope == "" {
			continue
		}
		for name, entry := range layer {
			r.nextToken++
			entry.token = r.nextToken
			layer[name] = entry
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
	definitions map[string]Definition
	version     uint64
	tools       map[string]Tool
}

// Acquire returns a frozen visible binding. The returned map is private and
// never aliases registry layers or caller-owned maps.
func (r *Registry) Acquire() *Lease {
	if r == nil {
		return &Lease{tools: map[string]Tool{}}
	}
	r.mu.RLock()
	view := make(map[string]Tool)
	definitions := make(map[string]Definition)
	for _, scope := range r.scopes {
		for name, entry := range r.layers[scope] {
			view[name] = entry.tool
			definitions[name] = entry.definition
		}
	}
	version := r.version
	r.mu.RUnlock()
	return &Lease{version: version, tools: view, definitions: definitions}
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

func (l *Lease) Definition(name string) (Definition, bool) {
	if l == nil {
		return Definition{}, false
	}
	definition, ok := l.definitions[name]
	definition.Parameters = jsonvalue.Clone(definition.Parameters)
	return definition, ok
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
		definition, ok := l.Definition(name)
		if !ok {
			continue
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": definition.Name, "description": definition.Description, "parameters": definition.Parameters,
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
	return r.Acquire().SchemasFiltered(keep)
}

// SchemaJSON returns a detached copy of the registration-time schema. Catalog
// refresh does not re-encode or recursively copy maps for every subscriber.
func (l *Lease) SchemaJSON(name string) ([]byte, bool) {
	if l == nil {
		return nil, false
	}
	definition, ok := l.definitions[name]
	return append([]byte(nil), definition.schemaJSON...), ok
}
