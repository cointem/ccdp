// Package plugin provides model routing and in-process tool hooks.
package plugin

import (
	"crypto/sha256"
	"sort"
	"sync"

	"ccdp/internal/llm"
)

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

// RunPreTool combines pre-tool hooks with deny > ask > allow. The
// hook list is copied under the lock so concurrent disposers (which move
// elements in place) never race with the iteration.
func (g *GoHooks) RunPreTool(toolName string, args map[string]any) (ToolDecision, string) {
	g.mu.RLock()
	hooks := make([]*preHookNode, len(g.pre))
	copy(hooks, g.pre)
	g.mu.RUnlock()
	decision, reason := DecisionNone, ""
	for _, node := range hooks {
		next, nextReason := node.h(toolName, args)
		if next == DecisionDeny {
			return next, nextReason
		}
		if next == DecisionAsk || next == DecisionAllow && decision == DecisionNone {
			decision, reason = next, nextReason
		}
	}
	return decision, reason
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
