package tui

import (
	"strings"
	"testing"
)

// /context reports only authoritative numbers: the live context estimate, the
// model window, and cumulative request accounting. It must not fabricate a
// per-section split or print a percentage.
func TestRenderContextUsesHonestNumbers(t *testing.T) {
	m := astraModel(t, 80, 24)
	out := m.renderContext()
	for _, want := range []string{"window:", "200k", "in use:", "12.3k", "free:", "187.7k"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context report missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"%", "system:", "tools:", "messages:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("context report fabricated %q:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "[") || !strings.Contains(out, "]") {
		t.Fatalf("context report is missing the proportion bar:\n%s", out)
	}
}

// Without a reported window the view degrades gracefully instead of dividing by
// zero or inventing a limit.
func TestRenderContextWithoutWindow(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.Settings.ContextWindow = 0
	out := m.renderContext()
	if !strings.Contains(out, "unknown") {
		t.Fatalf("missing window should be reported as unknown:\n%s", out)
	}
	if strings.Contains(out, "[") {
		t.Fatalf("bar should be omitted without a window:\n%s", out)
	}
}

// The bar saturates at the window and shows any non-zero use as at least one cell.
func TestContextBarBounds(t *testing.T) {
	if got := contextBar(0, 100); strings.Count(got, "#") != 0 {
		t.Fatalf("zero use filled the bar: %q", got)
	}
	if got := contextBar(1, 100000); strings.Count(got, "#") != 1 {
		t.Fatalf("tiny use should fill exactly one cell: %q", got)
	}
	if got := contextBar(100, 100); strings.Count(got, "#") != 20 {
		t.Fatalf("full window should saturate the bar: %q", got)
	}
	if got := contextBar(500, 100); strings.Count(got, "#") != 20 {
		t.Fatalf("overflow should clamp to the bar width: %q", got)
	}
	if got := contextBar(10, 0); got != "" {
		t.Fatalf("no window should render no bar: %q", got)
	}
}

// Running /context pushes a system report into the transcript.
func TestContextCommandPushesReport(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.runCommand("/context")
	var found bool
	for _, r := range m.reports {
		if r.kind == "system" && strings.Contains(r.text, "Context (model") {
			found = true
		}
	}
	if !found {
		t.Fatalf("/context did not push a context report; reports=%v", m.reports)
	}
}
