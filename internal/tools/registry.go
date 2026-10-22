package tools

import (
	"ccdp/internal/jsonvalue"
	"ccdp/internal/toolschema"
	"encoding/json"
	"fmt"

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
	OutputSchema      map[string]any
	ProgramCallable   bool
	schemaJSON        []byte
	input             *toolschema.Schema
	output            *toolschema.Schema
	inputError        error
	outputError       error
	externalInput     bool
}

// ValidationError explains why one registered tool is unavailable. Broken external
// schemas never enter a provider request alongside otherwise valid tools.
func (d Definition) ValidationError() error {
	if d.inputError != nil {
		return fmt.Errorf("%s input schema: %w", d.Name, d.inputError)
	}
	if d.outputError != nil {
		return fmt.Errorf("%s output schema: %w", d.Name, d.outputError)
	}
	return nil
}

type registryEntry struct {
	tool       Tool
	token      uint64
	definition Definition
}

func prepareRegistryEntry(tool Tool) registryEntry {
	definition := Definition{Name: tool.Name(), Description: tool.Description(), Parameters: jsonvalue.Clone(tool.Parameters())}
	if t, ok := tool.(ProgramTool); ok {
		definition.ProgramCallable = t.ProgramCallable()
	}
	inputSchema := definition.Parameters
	if t, ok := tool.(InputDefiner); ok {
		inputSchema = t.InputSchema()
		definition.externalInput = true
	}
	if t, ok := tool.(OutputDefiner); ok {
		definition.OutputSchema = jsonvalue.Clone(t.OutputSchema())
	}
	definition.input, definition.inputError = toolschema.Compile(inputSchema)
	if definition.OutputSchema != nil {
		definition.output, definition.outputError = toolschema.CompileValidation(definition.OutputSchema)
	}
	var err error
	definition.schemaJSON, err = json.Marshal(definition.Parameters)
	if err != nil {
		definition.inputError = fmt.Errorf("encode input schema: %w", err)
	}
	return registryEntry{tool: tool, definition: definition}
}

// PrepareArguments uses the exact registration frozen in this lease.
func (l *Lease) PrepareArguments(name string, args map[string]any) (map[string]any, error) {
	definition, ok := l.definitions[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if err := definition.ValidationError(); err != nil {
		return nil, err
	}
	if !definition.externalInput {
		return definition.input.Prepare(args)
	}
	// MCP/custom schemas remain rooted exactly as supplied. Host metadata is
	// validated separately and never sent to the external implementation.
	business := jsonvalue.Clone(args)
	capabilities, hasCapabilities := business["requested_capabilities"]
	delete(business, "requested_capabilities")
	if hasCapabilities {
		if err := capabilityArguments.Validate(map[string]any{"requested_capabilities": capabilities}); err != nil {
			return nil, fmt.Errorf("requested_capabilities: %w", err)
		}
	}
	prepared, err := definition.input.Prepare(business)
	if err != nil {
		return nil, err
	}
	if hasCapabilities {
		prepared["requested_capabilities"] = capabilities
	}
	return prepared, nil
}

// ValidateOutput never executes the tool and never rewrites its output.
func (l *Lease) ValidateOutput(name string, data json.RawMessage) error {
	definition, ok := l.definitions[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	if definition.outputError != nil {
		return fmt.Errorf("%s output schema: %w", name, definition.outputError)
	}
	if definition.output == nil {
		if len(data) == 0 {
			return nil
		}
		_, err := toolschema.Decode(data)
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("tool declared an output schema but returned no structured data")
	}
	return definition.output.ValidateJSON(data)
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
	definition.OutputSchema = jsonvalue.Clone(definition.OutputSchema)
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
		if !ok || definition.ValidationError() != nil {
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
