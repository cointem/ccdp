package plugin

import (
	"context"
	"strings"
	"testing"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/tools"
)

// newTestCtx builds a Context wired like the agent's.
func newTestCtx() *Context {
	return &Context{
		Events:  events.NewBus(),
		Tools:   tools.NewRegistry(),
		Models:  NewModelRegistry(),
		Hooks:   NewGoHooks(),
		Session: NewSessionRegistry(),
	}
}

// ---------- Host: dependency ordering ----------

type recordPlugin struct {
	name     string
	requires []string
	initLog  *[]string
	initErr  error
}

func (p *recordPlugin) Name() string       { return p.name }
func (p *recordPlugin) Requires() []string { return p.requires }
func (p *recordPlugin) Init(*Context) error {
	*p.initLog = append(*p.initLog, "init:"+p.name)
	return p.initErr
}
func (p *recordPlugin) Deinit() error { *p.initLog = append(*p.initLog, "deinit:"+p.name); return nil }

// registerBuiltin makes a plugin resolvable by name for the duration of a test.
func registerBuiltin(t *testing.T, p Plugin) {
	t.Helper()
	RegisterBuiltin(p)
	t.Cleanup(func() { delete(knownPlugins, p.Name()) })
}

func TestHostLoadsDependenciesFirst(t *testing.T) {
	h := NewHost(newTestCtx())
	var log []string
	base := &recordPlugin{name: "base", initLog: &log}
	app := &recordPlugin{name: "app", requires: []string{"base"}, initLog: &log}
	registerBuiltin(t, base)

	if err := h.Load(app); err != nil {
		t.Fatalf("Load(app): %v", err)
	}
	if len(log) != 2 || !strings.HasPrefix(log[0], "init:base") || !strings.HasPrefix(log[1], "init:app") {
		t.Fatalf("expected base before app, got %v", log)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close deinits in reverse load order: app then base.
	if len(log) != 4 || !strings.HasPrefix(log[2], "deinit:app") || !strings.HasPrefix(log[3], "deinit:base") {
		t.Fatalf("expected app before base on close, got %v", log)
	}
}

func TestHostRollsBackOnInitError(t *testing.T) {
	h := NewHost(newTestCtx())
	var log []string
	good := &recordPlugin{name: "good", initLog: &log}
	bad := &recordPlugin{name: "bad", requires: []string{"good"}, initLog: &log, initErr: errors_boom()}
	registerBuiltin(t, good)

	if err := h.Load(bad); err == nil {
		t.Fatal("expected Load to fail")
	}
	if names := h.Names(); len(names) != 0 {
		t.Fatalf("expected empty host after rollback, got %v", names)
	}
	// good must have been deinitialized during rollback (bad logged its own init).
	if len(log) != 3 || log[0] != "init:good" || log[2] != "deinit:good" {
		t.Fatalf("expected good init then deinit, got %v", log)
	}
}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

func errors_boom() error { return boomErr{} }

func TestHostDetectsCycles(t *testing.T) {
	h := NewHost(newTestCtx())
	var log []string
	a := &recordPlugin{name: "a", requires: []string{"b"}, initLog: &log}
	b := &recordPlugin{name: "b", requires: []string{"a"}, initLog: &log}
	registerBuiltin(t, b)
	if err := h.Load(a); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestHostUnloadCascadesDependents(t *testing.T) {
	h := NewHost(newTestCtx())
	var log []string
	base := &recordPlugin{name: "base", initLog: &log}
	app := &recordPlugin{name: "app", requires: []string{"base"}, initLog: &log}
	if err := h.Load(base); err != nil {
		t.Fatalf("Load(base): %v", err)
	}
	if err := h.Load(app); err != nil {
		t.Fatalf("Load(app): %v", err)
	}
	// Unloading a depended-upon plugin cascades: the dependent is unloaded
	// first (so it never sees a torn-down dependency), then the dependency.
	if err := h.Unload("base"); err != nil {
		t.Fatalf("Unload(base): %v", err)
	}
	if names := h.Names(); len(names) != 0 {
		t.Fatalf("expected empty host after cascade, got %v", names)
	}
	if len(log) != 4 || log[2] != "deinit:app" || log[3] != "deinit:base" {
		t.Fatalf("expected dependent teardown before dependency, got %v", log)
	}
}

func TestHostPendingPluginLoadsWhenDependencyArrives(t *testing.T) {
	h := NewHost(newTestCtx())
	var log []string
	base := &recordPlugin{name: "zbase", initLog: &log}
	app := &recordPlugin{name: "aapp", requires: []string{"zbase"}, initLog: &log}

	// The dependency is not available yet: the plugin stays pending and Load
	// reports the missing requirement, but keeps it registered.
	if err := h.Load(app); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("expected pending error, got %v", err)
	}
	if got := h.Pending(); len(got) != 1 || got[0] != "aapp" {
		t.Fatalf("expected aapp pending, got %v", got)
	}

	// Registering the dependency (even under a different name) satisfies the
	// requirement on the next Load and auto-loads the pending plugin.
	registerBuiltin(t, base)
	if err := h.Load(&recordPlugin{name: "unrelated", initLog: &log}); err != nil {
		t.Fatalf("Load(unrelated): %v", err)
	}
	if got := h.Pending(); len(got) != 0 {
		t.Fatalf("expected no pending plugins after dependency arrived, got %v", got)
	}
	found := false
	for _, n := range h.Names() {
		if n == "aapp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("aapp should have auto-loaded, got %v", h.Names())
	}
}

// ---------- Scoped tool registry ----------

type stubTool struct{ name string }

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub " + s.name }
func (s *stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s *stubTool) Run(*tools.Context) (string, error) { return "ran " + s.name, nil }

func TestScopedToolRegistry(t *testing.T) {
	reg := tools.NewRegistry()
	base := &stubTool{name: "Base"}
	override := &stubTool{name: "Base"} // same name, later scope
	ext := &stubTool{name: "Ext"}

	reg.RegisterIn("builtin", base)
	reg.RegisterIn("plugin-x", override)
	reg.RegisterIn("plugin-x", ext)

	// Later scope wins on collision.
	got, _ := reg.Get("Base")
	if got != override {
		t.Fatal("expected later scope to override earlier one")
	}
	if _, ok := reg.Get("Ext"); !ok {
		t.Fatal("expected Ext to be visible")
	}
	if n := len(reg.Names()); n != 2 {
		t.Fatalf("expected 2 names, got %d", n)
	}

	// Unregistering a scope removes all its tools.
	reg.UnregisterScope("plugin-x")
	if _, ok := reg.Get("Ext"); ok {
		t.Fatal("Ext should be gone after scope removal")
	}
	if got, _ := reg.Get("Base"); got != base {
		t.Fatal("Base should fall back to the builtin layer")
	}
}

func TestRegisterDisposer(t *testing.T) {
	reg := tools.NewRegistry()
	dispose := reg.RegisterIn("tmp", &stubTool{name: "Tmp"})
	if _, ok := reg.Get("Tmp"); !ok {
		t.Fatal("tool should be registered")
	}
	dispose()
	if _, ok := reg.Get("Tmp"); ok {
		t.Fatal("tool should be gone after dispose")
	}
}

// ---------- ModelRegistry ----------

type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	return llm.StreamResult{}, nil
}

func TestModelRegistryResolveRoute(t *testing.T) {
	r := NewModelRegistry()
	a := &fakeProvider{name: "p-a"}
	b := &fakeProvider{name: "p-b"}
	r.Register(a)
	r.Register(b)
	// No route and two providers → ambiguous → nil.
	if p, ok := r.ResolveRoute("model-x"); ok || p != nil {
		t.Fatal("expected no route for ambiguous resolve")
	}
	r.Route("model-x", "p-b")
	if p, ok := r.ResolveRoute("model-x"); !ok || p != b {
		t.Fatal("expected routed provider p-b")
	}
	r.Unregister("p-b")
	// The model route was removed; no implicit sole-provider fallback is allowed.
	if p, ok := r.ResolveRoute("model-x"); ok || p != nil {
		t.Fatal("expected no route after unregistering p-b")
	}
	if p, ok := r.ResolveRoute("p-a"); !ok || p != a {
		t.Fatal("expected exact provider-name route")
	}
}

func TestModelRegistryDisposer(t *testing.T) {
	r := NewModelRegistry()
	dispose := r.Register(&fakeProvider{name: "temp"})
	if len(r.Names()) != 1 {
		t.Fatal("expected one provider")
	}
	dispose()
	if len(r.Names()) != 0 {
		t.Fatal("provider should be gone after dispose")
	}
}

func TestModelRegistryDisposerDoesNotRemoveReplacement(t *testing.T) {
	r := NewModelRegistry()
	first := &fakeProvider{name: "same"}
	second := &fakeProvider{name: "same"}
	oldDispose := r.Register(first)
	r.Route("alias", "same")
	newDispose := r.Register(second)
	oldDispose()
	if got, ok := r.ResolveRoute("alias"); !ok || got != second {
		t.Fatalf("old disposer removed replacement route: provider=%v ok=%v", got, ok)
	}
	newDispose()
	if got, ok := r.ResolveRoute("alias"); ok || got != nil {
		t.Fatalf("replacement disposer left route: provider=%v ok=%v", got, ok)
	}
}

func TestModelRegistryFreezeKeepsOldProvider(t *testing.T) {
	r := NewModelRegistry()
	first := &fakeProvider{name: "same"}
	r.Register(first)
	r.Route("alias", "same")
	frozen := r.Freeze()
	second := &fakeProvider{name: "same"}
	r.Register(second)
	if got, ok := frozen.ResolveRoute("alias"); !ok || got != first {
		t.Fatalf("frozen registry changed after replacement: provider=%v ok=%v", got, ok)
	}
	if got, ok := r.ResolveRoute("alias"); !ok || got != second {
		t.Fatalf("live registry did not follow replacement: provider=%v ok=%v", got, ok)
	}
}

func TestModelRegistryDefaultRouteMatchesConnection(t *testing.T) {
	r := NewModelRegistry()
	p := &fakeProvider{name: "model"}
	r.Register(p)
	r.RouteDefault("model", "model", "https://a.example/v1", "key-a")
	if got, ok := r.ResolveDefault("model", "https://a.example/v1", "key-a"); !ok || got != p {
		t.Fatalf("expected matching default route: provider=%v ok=%v", got, ok)
	}
	if got, ok := r.ResolveDefault("model", "https://b.example/v1", "key-b"); ok || got != nil {
		t.Fatalf("stale default route reused: provider=%v ok=%v", got, ok)
	}
}

// ---------- GoHooks ----------

func TestGoHooksPreVerdict(t *testing.T) {
	g := NewGoHooks()
	g.AddPreTool(func(name string, args map[string]any) (ToolDecision, string) {
		if name == "Blocked" {
			return DecisionDeny, "blocked for test"
		}
		return DecisionNone, ""
	})
	if dec, reason := g.RunPreTool("Blocked", nil); dec != DecisionDeny || reason != "blocked for test" {
		t.Fatalf("expected deny verdict, got %v %q", dec, reason)
	}
	if dec, _ := g.RunPreTool("Other", nil); dec != DecisionNone {
		t.Fatalf("expected no verdict, got %v", dec)
	}
}

func TestGoHooksDisposer(t *testing.T) {
	g := NewGoHooks()
	dispose := g.AddPreTool(func(name string, args map[string]any) (ToolDecision, string) {
		return DecisionAllow, "x"
	})
	dispose()
	if dec, _ := g.RunPreTool("Any", nil); dec != DecisionNone {
		t.Fatal("hook should be removed after dispose")
	}
}

func TestGoHooksDisposerOrderIndependent(t *testing.T) {
	g := NewGoHooks()
	var ran []string
	d1 := g.AddPreTool(func(string, map[string]any) (ToolDecision, string) {
		ran = append(ran, "one")
		return DecisionNone, ""
	})
	d2 := g.AddPreTool(func(string, map[string]any) (ToolDecision, string) {
		ran = append(ran, "two")
		return DecisionNone, ""
	})
	// Dispose the first registration out of order: the remaining hook must
	// still run and still be removable afterwards.
	d1()
	g.RunPreTool("Any", nil)
	if len(ran) != 1 || ran[0] != "two" {
		t.Fatalf("expected only the remaining hook to run, got %v", ran)
	}
	d2()
	ran = nil
	g.RunPreTool("Any", nil)
	if len(ran) != 0 {
		t.Fatalf("remaining hook should be removable, got %v", ran)
	}
}

// ---------- Event bus ----------

func TestBusSubscribeEmitAndDispose(t *testing.T) {
	b := events.NewBus()
	var got []string
	dispose := b.Subscribe(events.TopicToolResult, func(topic events.Topic, p events.Payload) {
		got = append(got, "first")
	})
	b.Subscribe(events.TopicToolResult, func(topic events.Topic, p events.Payload) {
		got = append(got, "second")
	})
	b.Emit(events.TopicToolResult, nil)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("expected ordered delivery, got %v", got)
	}
	dispose()
	got = nil
	b.Emit(events.TopicToolResult, nil)
	if len(got) != 1 || got[0] != "second" {
		t.Fatalf("expected only the remaining subscriber, got %v", got)
	}
}

func TestBusIsolatesPanics(t *testing.T) {
	b := events.NewBus()
	b.Subscribe(events.TopicToolResult, func(topic events.Topic, p events.Payload) {
		panic("boom")
	})
	b.Subscribe(events.TopicToolResult, func(topic events.Topic, p events.Payload) {
		// This must still run despite the panicking subscriber.
		_ = topic
	})
	b.Emit(events.TopicToolResult, nil) // must not panic
	if b.Subscribers(events.TopicToolResult) != 2 {
		t.Fatal("subscribers should survive a panicking handler")
	}
}

// ---------- Session callbacks ----------

func TestSessionCallbacks(t *testing.T) {
	s := NewSessionRegistry()
	var created, resumed, disposed []string
	s.OnCreate(func(id, ws string) { created = append(created, id+"@"+ws) })
	s.OnResume(func(id string) { resumed = append(resumed, id) })
	s.OnDispose(func(id string) { disposed = append(disposed, id) })

	s.RunCreate("s1", "/ws")
	s.RunResume("s1")
	s.RunDispose("s1")
	if len(created) != 1 || created[0] != "s1@/ws" {
		t.Fatalf("unexpected create: %v", created)
	}
	if len(resumed) != 1 || resumed[0] != "s1" {
		t.Fatalf("unexpected resume: %v", resumed)
	}
	if len(disposed) != 1 || disposed[0] != "s1" {
		t.Fatalf("unexpected dispose: %v", disposed)
	}
}

func TestSessionCallbacksDisposerOrderIndependent(t *testing.T) {
	s := NewSessionRegistry()
	var created []string
	d1 := s.OnCreate(func(id, ws string) { created = append(created, "one") })
	d2 := s.OnCreate(func(id, ws string) { created = append(created, "two") })

	// Dispose the first registration out of order: the remaining callback
	// must still run and still be removable afterwards.
	d1()
	s.RunCreate("s1", "/ws")
	if len(created) != 1 || created[0] != "two" {
		t.Fatalf("expected only the remaining callback, got %v", created)
	}
	d2()
	created = nil
	s.RunCreate("s2", "/ws")
	if len(created) != 0 {
		t.Fatalf("remaining callback should be removable, got %v", created)
	}
}
