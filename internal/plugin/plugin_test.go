package plugin

import (
	"context"
	"testing"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/tools"
)

// ---------- Scoped tool registry ----------

type stubTool struct{ name string }

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub " + s.name }
func (s *stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s *stubTool) Execute(ctx *tools.Context, args map[string]any) (tools.Result, error) {
	ctx = ctx.WithArgs(args)
	text, err := func() (string, error) { return "ran " + s.name, nil }()
	return tools.Result{Text: text}, err
}

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
	r.RouteDefault("model", "model", HTTPBinding{Endpoint: "https://a.example/v1", APIKey: "key-a", Wire: "chat"})
	if got, ok := r.ResolveDefault("model", HTTPBinding{Endpoint: "https://a.example/v1", APIKey: "key-a", Wire: "chat"}); !ok || got != p {
		t.Fatalf("expected matching default route: provider=%v ok=%v", got, ok)
	}
	if got, ok := r.ResolveDefault("model", HTTPBinding{Endpoint: "https://b.example/v1", APIKey: "key-b", Wire: "chat"}); ok || got != nil {
		t.Fatalf("stale default route reused: provider=%v ok=%v", got, ok)
	}
	if got, ok := r.ResolveDefault("model", HTTPBinding{Endpoint: "https://a.example/v1", APIKey: "key-a", Wire: "responses"}); ok || got != nil {
		t.Fatalf("adapter speaking the previous wire format was reused: provider=%v ok=%v", got, ok)
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
