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
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
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

// Host loads and unloads plugins with dependency ordering. It implements the
// availability-driven (epoch fingerprint) model of deepseek-harness's Cordis
// kernel: a plugin is loaded whenever all of its Requires are loaded, and
// automatically unloaded again when any requirement disappears — regardless of
// the order Load calls arrived in. Plugins whose requirements are not (yet)
// met stay registered as "pending" and are loaded automatically once their
// dependencies appear.
type Host struct {
	ctx    *Context
	mu     sync.Mutex
	wanted map[string]Plugin // registered, may be loaded or pending
	loaded map[string]bool
	order  []string // load order (topological, stable)
}

// NewHost builds an empty host over a Context.
func NewHost(ctx *Context) *Host {
	return &Host{
		ctx:    ctx,
		wanted: map[string]Plugin{},
		loaded: map[string]bool{},
	}
}

// Load registers p and brings the host to a consistent state (loading p and
// any missing built-in dependencies, in dependency order). Requirements that
// are not yet satisfiable leave the plugin pending — the error is reported
// but the plugin stays registered and loads automatically once the
// dependency shows up. A failed Init rolls back everything this call loaded
// and drops the plugin entirely; a dependency cycle is rejected outright.
func (h *Host) Load(p Plugin) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p == nil || p.Name() == "" {
		return fmt.Errorf("plugin: name is required")
	}
	if _, ok := h.wanted[p.Name()]; ok {
		return fmt.Errorf("plugin: %q already registered", p.Name())
	}
	if err := h.detectCycle(p); err != nil {
		return err
	}
	start := len(h.order)
	h.wanted[p.Name()] = p
	if err := h.refresh(start, true); err != nil {
		return err
	}
	if !h.loaded[p.Name()] {
		return fmt.Errorf("plugin: %q pending — unsatisfied requirements: %s", p.Name(), strings.Join(h.missingFor(p), ", "))
	}
	return nil
}

// detectCycle walks the requirement graph through wanted/known plugins and
// rejects a plugin that (transitively) requires itself.
func (h *Host) detectCycle(p Plugin) error {
	var walk func(name string, path []string) error
	seen := map[string]bool{}
	walk = func(name string, path []string) error {
		if name == p.Name() {
			return fmt.Errorf("plugin: dependency cycle involving %q", p.Name())
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		next, ok := h.wanted[name]
		if !ok {
			if kp, kok := knownPlugins[name]; kok {
				next = kp
			} else {
				return nil
			}
		}
		for _, dep := range next.Requires() {
			if err := walk(dep, append(path, name)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, dep := range p.Requires() {
		if err := walk(dep, nil); err != nil {
			return err
		}
	}
	return nil
}

// refresh drives the host to a fixpoint (the Cordis epoch step): load every
// pending plugin whose requirements are now met, then unload every loaded
// plugin whose requirements no longer are, repeating until stable. On an Init
// failure everything loaded during this refresh is rolled back and the broken
// plugin is dropped from the host.
func (h *Host) refresh(start int, allowLoads bool) error {
	// Load pass — deterministic order (sorted names; map iteration would be
	// random), passes repeat until no progress is made. Missing requirements
	// that are known built-ins are pulled in automatically, like the old
	// recursive loadDeps did. Passes make progress either by loading a plugin
	// or by auto-registering one, so a fresh registration always gets its own
	// pass on the next iteration. Skipped during an Unload cascade: loading
	// pending plugins whose dependencies happen to be met would init them
	// just to deinit them again in the same pass.
	if allowLoads {
		for {
			progress := false
			for _, name := range h.wantedOrder() {
				if h.loaded[name] {
					continue
				}
				p := h.wanted[name]
				missing := h.missingFor(p)
				// Auto-register known built-ins for unsatisfied requirements.
				for _, dep := range missing {
					if _, wanted := h.wanted[dep]; !wanted {
						if kp, ok := knownPlugins[dep]; ok {
							h.wanted[dep] = kp
							progress = true
						}
					}
				}
				if len(h.missingFor(p)) > 0 {
					continue // still pending
				}
				if err := p.Init(h.ctx); err != nil {
					h.rollback(start)
					delete(h.wanted, name)
					return fmt.Errorf("plugin %q init: %w", name, err)
				}
				h.loaded[name] = true
				h.order = append(h.order, name)
				progress = true
			}
			if !progress {
				break
			}
		}
	}
	// Unload pass — requirements vanished (an Unload cascade): deinit in
	// reverse load order, repeat until stable. Unloaded plugins stay wanted
	// and reload automatically if their dependencies come back.
	for {
		progress := false
		for i := len(h.order) - 1; i >= 0; i-- {
			name := h.order[i]
			if !h.loaded[name] {
				continue
			}
			if len(h.missingFor(h.wanted[name])) > 0 {
				_ = h.wanted[name].Deinit()
				h.loaded[name] = false
				h.order = append(h.order[:i], h.order[i+1:]...)
				progress = true
				break // order shifted; restart the scan
			}
		}
		if !progress {
			break
		}
	}
	return nil
}

// missingFor lists the plugin's requirements that are not currently loaded.
func (h *Host) missingFor(p Plugin) []string {
	var missing []string
	for _, dep := range p.Requires() {
		if !h.loaded[dep] {
			missing = append(missing, dep)
		}
	}
	return missing
}

// wantedOrder returns the wanted plugin names in registration order
// (deterministic load passes; map iteration would be random).
func (h *Host) wantedOrder() []string {
	names := make([]string, 0, len(h.wanted))
	for n := range h.wanted {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// rollback deinitializes and removes every plugin loaded after index start.
func (h *Host) rollback(start int) {
	if start > len(h.order) {
		start = len(h.order) // the unload pass may have shrunk the order
	}
	for i := len(h.order) - 1; i >= start; i-- {
		name := h.order[i]
		if p, ok := h.wanted[name]; ok && h.loaded[name] {
			_ = p.Deinit()
			h.loaded[name] = false
		}
	}
	h.order = h.order[:start]
}

// Unload removes a plugin from the host. Plugins that depend on it are
// unloaded too (Cordis cascade semantics): dependents tear down first (in
// reverse load order) so they never see a torn-down dependency, then the
// plugin itself. Dependents stay registered and reload automatically if the
// dependency is ever added back.
func (h *Host) Unload(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.wanted[name]
	if !ok {
		return fmt.Errorf("plugin: %q not loaded", name)
	}
	wasLoaded := h.loaded[name]
	start := len(h.order)
	// Take the plugin out of the host first; the unload pass below then
	// cascades its dependents down without touching it again.
	delete(h.wanted, name)
	delete(h.loaded, name)
	for i, n := range h.order {
		if n == name {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
	// No load pass here: loading pending plugins whose dependencies happen to
	// be met would init them just to deinit them again in the same cascade.
	if err := h.refresh(start, false); err != nil {
		return err
	}
	if wasLoaded {
		if err := p.Deinit(); err != nil {
			return fmt.Errorf("plugin %q deinit: %w", name, err)
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

// Pending returns registered plugin names that are not loaded because their
// requirements are not met (waiting for a dependency to appear).
func (h *Host) Pending() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, name := range h.wantedOrder() {
		if !h.loaded[name] {
			out = append(out, name)
		}
	}
	return out
}

// Close deinits all loaded plugins in reverse load order.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []string
	for i := len(h.order) - 1; i >= 0; i-- {
		name := h.order[i]
		if err := h.wanted[name].Deinit(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	h.wanted = map[string]Plugin{}
	h.loaded = map[string]bool{}
	h.order = nil
	if len(errs) > 0 {
		return fmt.Errorf("plugin close: %s", joinErrs(errs))
	}
	return nil
}

// knownPlugins is the catalog of built-in plugins resolvable by name.
var knownPlugins = map[string]Plugin{}

// RegisterBuiltin makes a plugin available for dependency resolution by name.
func RegisterBuiltin(p Plugin) {
	if p != nil && p.Name() != "" {
		knownPlugins[p.Name()] = p
	}
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
	providers map[string]providerEntry // provider name -> provider
	routes    map[string]routeEntry    // model -> provider registration
	nextToken uint64
	frozen    bool
}

type providerEntry struct {
	provider llm.Provider
	token    uint64
}

type routeEntry struct {
	provider string
	token    uint64
	kind     string
	endpoint string
	keyHash  [32]byte
	wire     string
}

// HTTPBinding is the complete identity of a generated HTTP adapter for one
// model: the endpoint, the credential that authenticates it and the wire
// format spoken on it all come from the same config provider record. Passing
// them as one value keeps the cache lookup from treating a subset as an
// identity — an endpoint/key match with a changed wire_api would otherwise
// reuse an adapter that speaks the previous provider's protocol.
type HTTPBinding struct {
	Endpoint string
	APIKey   string
	Wire     string
}

// NewModelRegistry builds an empty registry.
func NewModelRegistry() *ModelRegistry {
	return &ModelRegistry{providers: map[string]providerEntry{}, routes: map[string]routeEntry{}}
}

// Register adds a provider under its own Name and returns a disposer.
func (r *ModelRegistry) Register(p llm.Provider) func() {
	if r == nil || p == nil || p.Name() == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.frozen {
		r.mu.Unlock()
		return func() {}
	}
	r.nextToken++
	token := r.nextToken
	name := p.Name()
	r.providers[name] = providerEntry{provider: p, token: token}
	// Re-registration replaces a provider in place. Existing model aliases
	// continue to follow that provider name, but point at the new registration
	// token so an old disposer cannot remove the replacement.
	for model, route := range r.routes {
		if route.provider == name {
			route.token = token
			r.routes[model] = route
		}
	}
	r.mu.Unlock()
	// A disposer is tied to this exact registration token. If the same
	// provider name was subsequently replaced, the old plugin must not remove
	// the replacement (the ABA bug in the previous name-only disposer).
	var once sync.Once
	return func() {
		once.Do(func() { r.unregisterToken(name, token) })
	}
}

// Unregister removes a provider and its model routes.
func (r *ModelRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	entry, ok := r.providers[name]
	if !ok {
		return
	}
	delete(r.providers, name)
	for m, route := range r.routes {
		if route.provider == name && route.token == entry.token {
			delete(r.routes, m)
		}
	}
}

func (r *ModelRegistry) unregisterToken(name string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	entry, ok := r.providers[name]
	if !ok || entry.token != token {
		return
	}
	delete(r.providers, name)
	for model, route := range r.routes {
		if route.provider == name && route.token == token {
			delete(r.routes, model)
		}
	}
}

// Route maps a model name to a provider name.
func (r *ModelRegistry) Route(model, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	if entry, ok := r.providers[provider]; ok {
		r.routes[model] = routeEntry{provider: provider, token: entry.token, kind: "explicit"}
	}
}

// RouteDefault maps a model to an adapter created from the model's own
// endpoint/key/wire configuration. The complete HTTPBinding lets a later reload
// replace a stale adapter without classifying providers by Go concrete type.
// The credential itself is never retained by the registry.
func (r *ModelRegistry) RouteDefault(model, provider string, binding HTTPBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	if entry, ok := r.providers[provider]; ok {
		r.routes[model] = routeEntry{provider: provider, token: entry.token, kind: "http", endpoint: binding.Endpoint, keyHash: sha256.Sum256([]byte(binding.APIKey)), wire: binding.Wire}
	}
}

// ResolveDefault returns an adapter only when its frozen endpoint, credential
// fingerprint and wire format still match. It is intentionally separate from
// ResolveRoute, which resolves explicit plugin routes regardless of HTTP
// configuration.
func (r *ModelRegistry) ResolveDefault(model string, binding HTTPBinding) (llm.Provider, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[model]
	if !ok || route.kind != "http" || route.endpoint != binding.Endpoint || route.wire != binding.Wire || route.keyHash != sha256.Sum256([]byte(binding.APIKey)) {
		return nil, false
	}
	entry, ok := r.providers[route.provider]
	if !ok || entry.token != route.token {
		return nil, false
	}
	return entry.provider, true
}

// ResolveRouteInfo resolves a model route without hiding its origin. The kind
// and endpoint are needed by the agent to distinguish a generated HTTP route
// from an explicit plugin route when a model's endpoint/key configuration is
// reloaded. A stale generated route must not be reused for the new connection.
func (r *ModelRegistry) ResolveRouteInfo(model string) (llm.Provider, string, string, bool) {
	if r == nil {
		return nil, "", "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if route, ok := r.routes[model]; ok {
		if entry, exists := r.providers[route.provider]; exists && entry.token == route.token {
			return entry.provider, route.kind, route.endpoint, true
		}
	}
	if entry, ok := r.providers[model]; ok {
		return entry.provider, "explicit", "", true
	}
	return nil, "", "", false
}

// ResolveRoute resolves only an explicit model route (or a provider registered
// under the model's exact name). It never applies the legacy sole-provider
// fallback. Agent request preparation uses this method whenever a model has a
// configured endpoint so a different model cannot accidentally inherit another
// provider's URL or credentials.
func (r *ModelRegistry) ResolveRoute(model string) (llm.Provider, bool) {
	p, _, _, ok := r.ResolveRouteInfo(model)
	return p, ok
}

// Clone returns a mutable point-in-time copy of the registry. Provider
// implementations themselves are intentionally shared; registration and route
// metadata are copied so a child can evolve its catalog independently.
func (r *ModelRegistry) Clone() *ModelRegistry {
	if r == nil {
		return NewModelRegistry()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := &ModelRegistry{
		providers: make(map[string]providerEntry, len(r.providers)),
		routes:    make(map[string]routeEntry, len(r.routes)),
		nextToken: r.nextToken,
	}
	for name, entry := range r.providers {
		clone.providers[name] = entry
	}
	for model, route := range r.routes {
		clone.routes[model] = route
	}
	return clone
}

// Freeze returns an immutable snapshot suitable for child/guardian work. A
// frozen registry still resolves providers but ignores later registrations,
// routes, and unregister calls; the child therefore cannot observe catalog
// churn in its parent while a request is being prepared.
func (r *ModelRegistry) Freeze() *ModelRegistry {
	clone := r.Clone()
	clone.frozen = true
	return clone
}

// Frozen is an alias for Freeze for callers that prefer noun-style naming.
func (r *ModelRegistry) Frozen() *ModelRegistry { return r.Freeze() }

// Names returns provider names in sorted order.
func (r *ModelRegistry) Names() []string {
	if r == nil {
		return nil
	}
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

// preHookNode and postHookNode let disposers remove their own hook by pointer
// identity: disposing in any order can never remove or shift someone else's
// registration.
type preHookNode struct{ h PreToolHook }

type postHookNode struct{ h PostToolHook }

// GoHooks is a registry of in-process hooks run alongside shell hooks.
type GoHooks struct {
	mu   sync.RWMutex
	pre  []*preHookNode
	post []*postHookNode
}

// NewGoHooks builds an empty hook registry.
func NewGoHooks() *GoHooks { return &GoHooks{} }

// AddPreTool registers a pre-tool hook and returns a disposer.
func (g *GoHooks) AddPreTool(h PreToolHook) func() {
	g.mu.Lock()
	node := &preHookNode{h: h}
	g.pre = append(g.pre, node)
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for i, s := range g.pre {
			if s == node {
				g.pre = append(g.pre[:i], g.pre[i+1:]...)
				return
			}
		}
	}
}

// SnapshotPreTool returns an immutable copy of the currently registered
// pre-tool decisions. Runtime child construction uses this at a frozen step
// boundary; post observers and lifecycle callbacks are intentionally not part
// of the child contract.
func (g *GoHooks) SnapshotPreTool() []PreToolHook {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	out := make([]PreToolHook, 0, len(g.pre))
	for _, node := range g.pre {
		if node != nil && node.h != nil {
			out = append(out, node.h)
		}
	}
	g.mu.RUnlock()
	return out
}

// AddPostTool registers a post-tool hook and returns a disposer.
func (g *GoHooks) AddPostTool(h PostToolHook) func() {
	g.mu.Lock()
	node := &postHookNode{h: h}
	g.post = append(g.post, node)
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for i, s := range g.post {
			if s == node {
				g.post = append(g.post[:i], g.post[i+1:]...)
				return
			}
		}
	}
}

// RunPreTool invokes all pre-tool hooks; the first non-none verdict wins. The
// hook list is copied under the lock so concurrent disposers (which move
// elements in place) never race with the iteration.
func (g *GoHooks) RunPreTool(toolName string, args map[string]any) (ToolDecision, string) {
	g.mu.RLock()
	hooks := make([]*preHookNode, len(g.pre))
	copy(hooks, g.pre)
	g.mu.RUnlock()
	for _, node := range hooks {
		dec, reason := node.h(toolName, args)
		if dec != DecisionNone {
			return dec, reason
		}
	}
	return DecisionNone, ""
}

// RunPostTool invokes all post-tool hooks and concatenates their additions.
func (g *GoHooks) RunPostTool(toolName string, args map[string]any, result string) string {
	g.mu.RLock()
	hooks := make([]*postHookNode, len(g.post))
	copy(hooks, g.post)
	g.mu.RUnlock()
	extra := ""
	for _, node := range hooks {
		if add := node.h(toolName, args, result); add != "" {
			extra += "\n" + add
		}
	}
	return extra
}

// ---------- SessionRegistry (session lifecycle callbacks) ----------

// createNode, resumeNode and disposeNode let disposers remove their own
// callback by pointer identity: disposing in any order can never remove or
// shift someone else's registration.
type createNode struct{ fn func(id, workspace string) }

type resumeNode struct{ fn func(id string) }

type disposeNode struct{ fn func(id string) }

// SessionRegistry lets plugins observe session create/resume/dispose.
type SessionRegistry struct {
	mu        sync.RWMutex
	onCreate  []*createNode
	onResume  []*resumeNode
	onDispose []*disposeNode
}

// NewSessionRegistry builds an empty registry.
func NewSessionRegistry() *SessionRegistry { return &SessionRegistry{} }

// OnCreate registers a callback for newly created sessions.
func (r *SessionRegistry) OnCreate(fn func(id, workspace string)) func() {
	r.mu.Lock()
	node := &createNode{fn: fn}
	r.onCreate = append(r.onCreate, node)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, s := range r.onCreate {
			if s == node {
				r.onCreate = append(r.onCreate[:i], r.onCreate[i+1:]...)
				return
			}
		}
	}
}

// OnResume registers a callback for resumed sessions.
func (r *SessionRegistry) OnResume(fn func(id string)) func() {
	r.mu.Lock()
	node := &resumeNode{fn: fn}
	r.onResume = append(r.onResume, node)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, s := range r.onResume {
			if s == node {
				r.onResume = append(r.onResume[:i], r.onResume[i+1:]...)
				return
			}
		}
	}
}

// OnDispose registers a callback for disposed sessions.
func (r *SessionRegistry) OnDispose(fn func(id string)) func() {
	r.mu.Lock()
	node := &disposeNode{fn: fn}
	r.onDispose = append(r.onDispose, node)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, s := range r.onDispose {
			if s == node {
				r.onDispose = append(r.onDispose[:i], r.onDispose[i+1:]...)
				return
			}
		}
	}
}

// RunCreate notifies create callbacks. The list is copied under the lock so
// concurrent disposers (which move elements in place) never race with the
// iteration.
func (r *SessionRegistry) RunCreate(id, workspace string) {
	r.mu.RLock()
	nodes := make([]*createNode, len(r.onCreate))
	copy(nodes, r.onCreate)
	r.mu.RUnlock()
	for _, node := range nodes {
		node.fn(id, workspace)
	}
}

// RunResume notifies resume callbacks.
func (r *SessionRegistry) RunResume(id string) {
	r.mu.RLock()
	nodes := make([]*resumeNode, len(r.onResume))
	copy(nodes, r.onResume)
	r.mu.RUnlock()
	for _, node := range nodes {
		node.fn(id)
	}
}

// RunDispose notifies dispose callbacks.
func (r *SessionRegistry) RunDispose(id string) {
	r.mu.RLock()
	nodes := make([]*disposeNode, len(r.onDispose))
	copy(nodes, r.onDispose)
	r.mu.RUnlock()
	for _, node := range nodes {
		node.fn(id)
	}
}
