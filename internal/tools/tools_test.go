package tools

import (
	"ccdp/internal/fsops"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

func scopedTestContext(t *testing.T, dir string) *Context {
	t.Helper()
	resources := NewResources("test:"+t.Name(), filepath.Join(dir, ".ccdp-session"))
	t.Cleanup(func() { _ = resources.Close() })
	return resources.Context(context.Background(), dir, sandbox.New(dir))
}

func TestWriteFileNoFollowRejectsHardlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "control.json")
	alias := filepath.Join(dir, "workspace-alias.json")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	v, _ := fsops.Observe(alias)
	if _, err := fsops.Publish(alias, []byte("changed\n"), 0o600, &v); err == nil {
		t.Fatal("writeFileNoFollow accepted a multiply-linked target")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Fatalf("hard-linked target was modified: %q", data)
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<html><head><title>x</title><style>a{}</style></head><body>
	<p>Hello <b>world</b></p><ul><li>one</li><li>two</li></ul><script>bad()</script>
	</body></html>`
	got := htmlToText(html)
	if strings.Contains(got, "bad") || strings.Contains(got, "<") {
		t.Errorf("htmlToText leaked markup: %q", got)
	}
	for _, want := range []string{"Hello", "world", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("htmlToText missing %q: %q", want, got)
		}
	}
}

func TestParseDuckDuckGo(t *testing.T) {
	html := `<div class="result">
	<a class="result__a" href="//example.com/a">Go Docs</a>
	<a class="result__snippet">The official Go documentation.</a>
	</div>
	<div class="result">
	<a class="result__a" href="//example.com/b">Go Blog</a>
	<a class="result__snippet">News from the Go team &amp; more.</a>
	</div>`
	results := parseDuckDuckGo(html, 10)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(results), results)
	}
	if results[0].Title != "Go Docs" || results[1].Snippet != "News from the Go team & more." {
		t.Errorf("unexpected parse: %+v", results)
	}
}

type registryTestTool struct{ name string }

func (t *registryTestTool) Name() string                 { return t.name }
func (t *registryTestTool) Description() string          { return "test" }
func (t *registryTestTool) Parameters() map[string]any   { return map[string]any{"type": "object"} }
func (t *registryTestTool) Run(*Context) (string, error) { return "", nil }

func TestRegistryDisposerDoesNotRemoveReplacement(t *testing.T) {
	r := NewRegistry()
	first := &registryTestTool{name: "same"}
	second := &registryTestTool{name: "same"}
	disposeFirst := r.RegisterIn("plugin", first)
	_ = r.RegisterIn("plugin", second)
	disposeFirst()
	got, ok := r.Get("same")
	if !ok || got != second {
		t.Fatalf("old disposer removed replacement: got=%v ok=%v", got, ok)
	}
}

func TestRegistryLeaseRetainsBindingAcrossReplacement(t *testing.T) {
	r := NewRegistry()
	old := &registryTestTool{name: "same"}
	newTool := &registryTestTool{name: "same"}
	_ = r.RegisterIn("plugin", old)
	lease := r.Acquire()
	_ = r.RegisterIn("plugin", newTool)
	current, _ := r.Get("same")
	bound, _ := lease.Get("same")
	if current != newTool || bound != old {
		t.Fatalf("lease mixed registry generations: current=%v bound=%v", current, bound)
	}
}
