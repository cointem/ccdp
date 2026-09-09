// Package plugin implements the extension host for ccdp, modeled on
// deepseek-harness's harness design and pi's runtime layering:
//
//   - A Context carries the domain registries plugins can use: events (tool and
//     session lifecycle bus), tools (scoped tool registry), models (LLM
//     provider registry), hooks (in-process Go hooks) and session callbacks.
//   - A Plugin declares its name and dependencies (Requires), so the Host loads
//     plugins in topological order and unloads them in reverse.
//   - Every registration returns a disposer, so plugins tear down cleanly.
//
// The agent itself is just another set of plugins: the built-in toolset and the
// default LLM provider are loaded through the same path an external plugin
// would use, which keeps the core thin and the extension surface uniform.
package plugin

import (
	"fmt"
	"sort"
	"sync"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/tools"
)

// Context is the host surface handed to every plugin at Init time. Fields are
// intentionally exported: plugins mutate the registries to extend ccdp.
type Context struct {
	Events  *events.Bus
	Tools   *tools.Registry
	Models  *ModelRegistry
	Hooks   *GoHooks
	Session *SessionRegistry
}

// Plugin is the extension contract.
type Plugin interface {
	// Name is a unique identifier (e.g. "builtin-tools").
	Name() string
	// Requires lists the names of other plugins that must be loaded first.
	Requires() []string
	// Init runs after all dependencies are loaded. Returning an error fails
	// the load; the host then rolls back already-initialized plugins.
	Init(ctx *Context) error
	// Deinit runs on unload or host close, in reverse load order.
	Deinit() error
}

// Host loads and unloads plugins with dependency ordering.
type Host struct {
	ctx     *Context
	mu      sync.Mutex
	plugins map[string]Plugin
	order   []string // load order (topological)
}

// NewHost builds an empty host over a Context.
func NewHost(ctx *Context) *Host {
	return &Host{ctx: ctx, plugins: map[string]Plugin{}}
}

// Load initializes p after its Requires plugins, which are loaded recursively
// if absent. On Init failure the plugins added by this call are deinitialized
// and removed so the host stays consistent.
func (h *Host) Load(p Plugin) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p == nil || p.Name() == "" {
		return fmt.Errorf("plugin: name is required")
	}
	if _, ok := h.plugins[p.Name()]; ok {
		return fmt.Errorf("plugin: %q already loaded", p.Name())
	}
	start := len(h.order)
	if err := h.loadDeps(p, map[string]bool{p.Name(): true}); err != nil {
		return err
	}
	if err := p.Init(h.ctx); err != nil {
		h.rollback(start)
		return fmt.Errorf("plugin %q init: %w", p.Name(), err)
	}
	h.plugins[p.Name()] = p
	h.order = append(h.order, p.Name())
	return nil
}

// rollback deinitializes and removes every plugin loaded after index start.
func (h *Host) rollback(start int) {
	for i := len(h.order) - 1; i >= start; i-- {
		name := h.order[i]
		if pl, ok := h.plugins[name]; ok {
			_ = pl.Deinit()
			delete(h.plugins, name)
		}
	}
	h.order = h.order[:start]
}

// loadDeps recursively loads Requires() plugins, detecting cycles.
func (h *Host) loadDeps(p Plugin, visiting map[string]bool) error {
	for _, dep := range p.Requires() {
		if _, ok := h.plugins[dep]; ok {
			continue
		}
		if visiting[dep] {
			return fmt.Errorf("plugin: dependency cycle involving %q", dep)
		}
		depPlugin, err := h.resolve(dep)
		if err != nil {
			return err
		}
		next := make(map[string]bool, len(visiting)+1)
		for k, v := range visiting {
			next[k] = v
		}
		next[dep] = true
		if err := h.loadDeps(depPlugin, next); err != nil {
			return err
		}
		if err := depPlugin.Init(h.ctx); err != nil {
			return fmt.Errorf("plugin %q init: %w", dep, err)
		}
		h.plugins[dep] = depPlugin
		h.order = append(h.order, dep)
	}
	return nil
}

// resolve looks up a dependency plugin by name. Built-in plugins are known to
// the host; unknown names are an error.
func (h *Host) resolve(name string) (Plugin, error) {
	if p, ok := knownPlugins[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plugin: unknown dependency %q", name)
}

// knownPlugins is the catalog of built-in plugins resolvable by name.
var knownPlugins = map[string]Plugin{}

// RegisterBuiltin makes a plugin available for dependency resolution by name.
func RegisterBuiltin(p Plugin) {
	if p != nil && p.Name() != "" {
		knownPlugins[p.Name()] = p
	}
}

// Unload removes a plugin, calling its Deinit. Plugins that depend on it are
// refused until they are unloaded first.
func (h *Host) Unload(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.plugins[name]
	if !ok {
		return fmt.Errorf("plugin: %q not loaded", name)
	}
	for _, other := range h.plugins {
		for _, dep := range other.Requires() {
			if dep == name && other.Name() != name {
				return fmt.Errorf("plugin: %q depends on %q; unload it first", other.Name(), name)
			}
		}
	}
	if err := p.Deinit(); err != nil {
		return fmt.Errorf("plugin %q deinit: %w", name, err)
	}
	delete(h.plugins, name)
	for i, n := range h.order {
		if n == name {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
	return nil
}

// Names returns loaded plugin names in load order.
func (h *Host) Names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.order))
	copy(out, h.order)
	return out
}

// Close deinits all plugins in reverse load order.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []string
	for i := len(h.order) - 1; i >= 0; i-- {
		name := h.order[i]
		if err := h.plugins[name].Deinit(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	h.plugins = map[string]Plugin{}
	h.order = nil
	if len(errs) > 0 {
		return fmt.Errorf("plugin close: %s", joinErrs(errs))
	}
	return nil
}

func joinErrs(errs []string) string {
	s := ""
	for i, e := range errs {
		if i > 0 {
			s += "; "
		}
		s += e
	}
	return s
}

// ---------- ModelRegistry (pi's ModelRegistry idea) ----------

// ModelRegistry manages LLM providers and maps model names to them. Providers
// can be registered and unregistered at runtime; the agent resolves the
// provider for the configured model through this registry.
type ModelRegistry struct {
	mu        sync.RWMutex
	providers map[string]llm.Provider // provider name -> provider
	routes    map[string]string       // model -> provider name
}

// NewModelRegistry builds an empty registry.
func NewModelRegistry() *ModelRegistry {
	return &ModelRegistry{providers: map[string]llm.Provider{}, routes: map[string]string{}}
}

// Register adds a provider under its own Name and returns a disposer.
func (r *ModelRegistry) Register(p llm.Provider) func() {
	if p == nil {
		return func() {}
	}
	r.mu.Lock()
	r.providers[p.Name()] = p
	r.mu.Unlock()
	return func() { r.Unregister(p.Name()) }
}

// Unregister removes a provider and its model routes.
func (r *ModelRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, name)
	for m, pn := range r.routes {
		if pn == name {
			delete(r.routes, m)
		}
	}
}

// Route maps a model name to a provider name.
func (r *ModelRegistry) Route(model, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[provider]; ok {
		r.routes[model] = provider
	}
}

// Resolve returns the provider serving the given model: the explicit route, or
// the sole registered provider, or nil.
func (r *ModelRegistry) Resolve(model string) llm.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pn, ok := r.routes[model]; ok {
		if p, ok := r.providers[pn]; ok {
			return p
		}
	}
	if len(r.providers) == 1 {
		for _, p := range r.providers {
			return p
		}
	}
	if p, ok := r.providers[model]; ok {
		return p
	}
	return nil
}

// Names returns provider names in sorted order.
func (r *ModelRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---------- GoHooks (in-process hooks, complementing shell hooks) ----------

// ToolDecision mirrors the shell-hook verdicts for Go-level hooks.
type ToolDecision int

const (
	// DecisionNone means the hook has no opinion; fall through.
	DecisionNone ToolDecision = iota
	DecisionAllow
	DecisionDeny
	DecisionAsk
)

// PreToolHook inspects a tool call before execution and may return a verdict.
type PreToolHook func(toolName string, args map[string]any) (ToolDecision, string)

// PostToolHook observes a tool result; the returned text is appended to it.
type PostToolHook func(toolName string, args map[string]any, result string) string

// GoHooks is a registry of in-process hooks run alongside shell hooks.
type GoHooks struct {
	mu   sync.RWMutex
	pre  []PreToolHook
	post []PostToolHook
}

// NewGoHooks builds an empty hook registry.
func NewGoHooks() *GoHooks { return &GoHooks{} }

// AddPreTool registers a pre-tool hook and returns a disposer.
func (g *GoHooks) AddPreTool(h PreToolHook) func() {
	g.mu.Lock()
	g.pre = append(g.pre, h)
	i := len(g.pre) - 1
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if i < len(g.pre) {
			g.pre = append(g.pre[:i], g.pre[i+1:]...)
		}
	}
}

// AddPostTool registers a post-tool hook and returns a disposer.
func (g *GoHooks) AddPostTool(h PostToolHook) func() {
	g.mu.Lock()
	g.post = append(g.post, h)
	i := len(g.post) - 1
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if i < len(g.post) {
			g.post = append(g.post[:i], g.post[i+1:]...)
		}
	}
}

// RunPreTool invokes all pre-tool hooks; the first non-none verdict wins.
func (g *GoHooks) RunPreTool(toolName string, args map[string]any) (ToolDecision, string) {
	g.mu.RLock()
	hooks := g.pre
	g.mu.RUnlock()
	for _, h := range hooks {
		dec, reason := h(toolName, args)
		if dec != DecisionNone {
			return dec, reason
		}
	}
	return DecisionNone, ""
}

// RunPostTool invokes all post-tool hooks and concatenates their additions.
func (g *GoHooks) RunPostTool(toolName string, args map[string]any, result string) string {
	g.mu.RLock()
	hooks := g.post
	g.mu.RUnlock()
	extra := ""
	for _, h := range hooks {
		if add := h(toolName, args, result); add != "" {
			extra += "\n" + add
		}
	}
	return extra
}

// ---------- SessionRegistry (session lifecycle callbacks) ----------

// SessionRegistry lets plugins observe session create/resume/dispose.
type SessionRegistry struct {
	mu        sync.RWMutex
	onCreate  []func(id, workspace string)
	onResume  []func(id string)
	onDispose []func(id string)
}

// NewSessionRegistry builds an empty registry.
func NewSessionRegistry() *SessionRegistry { return &SessionRegistry{} }

// OnCreate registers a callback for newly created sessions.
func (r *SessionRegistry) OnCreate(fn func(id, workspace string)) func() {
	r.mu.Lock()
	r.onCreate = append(r.onCreate, fn)
	i := len(r.onCreate) - 1
	r.mu.Unlock()
	return func() { r.mu.Lock(); defer r.mu.Unlock(); r.onCreate = append(r.onCreate[:i], r.onCreate[i+1:]...) }
}

// OnResume registers a callback for resumed sessions.
func (r *SessionRegistry) OnResume(fn func(id string)) func() {
	r.mu.Lock()
	r.onResume = append(r.onResume, fn)
	i := len(r.onResume) - 1
	r.mu.Unlock()
	return func() { r.mu.Lock(); defer r.mu.Unlock(); r.onResume = append(r.onResume[:i], r.onResume[i+1:]...) }
}

// OnDispose registers a callback for disposed sessions.
func (r *SessionRegistry) OnDispose(fn func(id string)) func() {
	r.mu.Lock()
	r.onDispose = append(r.onDispose, fn)
	i := len(r.onDispose) - 1
	r.mu.Unlock()
	return func() { r.mu.Lock(); defer r.mu.Unlock(); r.onDispose = append(r.onDispose[:i], r.onDispose[i+1:]...) }
}

// RunCreate notifies create callbacks.
func (r *SessionRegistry) RunCreate(id, workspace string) {
	r.mu.RLock()
	fns := r.onCreate
	r.mu.RUnlock()
	for _, fn := range fns {
		fn(id, workspace)
	}
}

// RunResume notifies resume callbacks.
func (r *SessionRegistry) RunResume(id string) {
	r.mu.RLock()
	fns := r.onResume
	r.mu.RUnlock()
	for _, fn := range fns {
		fn(id)
	}
}

// RunDispose notifies dispose callbacks.
func (r *SessionRegistry) RunDispose(id string) {
	r.mu.RLock()
	fns := r.onDispose
	r.mu.RUnlock()
	for _, fn := range fns {
		fn(id)
	}
}
