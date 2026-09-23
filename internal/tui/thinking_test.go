package tui

import (
	"strings"
	"testing"
)

func TestThinkingRenderingAndToggle(t *testing.T) {
	m := NewWithClient(nil, "/test", false)
	t.Cleanup(m.watchCancel)

	item := historyCell{
		kind:   "thinking",
		text:   "Step 1: analyze requirement\nStep 2: plan changes\nStep 3: write code",
		status: "success",
		toolID: "thought-1",
	}

	// Collapsed by default
	collapsed := m.renderLogItem(&item, 80)
	cleanCollapsed := sanitizeANSI(collapsed)
	if !strings.Contains(cleanCollapsed, "Thought") {
		t.Fatalf("collapsed thinking mismatch: %q", cleanCollapsed)
	}
	if strings.Contains(cleanCollapsed, "Step 1") || strings.Contains(cleanCollapsed, "Step 2") {
		t.Fatalf("collapsed thinking should not show Step 2: %q", cleanCollapsed)
	}

	// Expand
	m.width, m.height = 80, 24
	m.items = []historyCell{item}
	m.toggleToolExpanded("thought-1")
	expanded := m.renderDetailPopover()
	cleanExpanded := sanitizeANSI(expanded)
	if !strings.Contains(cleanExpanded, "[click to collapse]") {
		t.Fatalf("expanded thinking missing collapse hint: %q", cleanExpanded)
	}
	if !strings.Contains(cleanExpanded, "Step 1: analyze requirement") ||
		!strings.Contains(cleanExpanded, "Step 2: plan changes") ||
		!strings.Contains(cleanExpanded, "Step 3: write code") {
		t.Fatalf("expanded thinking missing steps:\n%s", cleanExpanded)
	}

	// Toggle back to collapsed
	m.toggleToolExpanded("thought-1")
	recollapsed := m.renderLogItem(&item, 80)
	cleanRecollapsed := sanitizeANSI(recollapsed)
	if strings.Contains(cleanRecollapsed, "Step 2") {
		t.Fatalf("recollapsed thinking should not show Step 2: %q", cleanRecollapsed)
	}
}

func TestThinkingInReader(t *testing.T) {
	entry := readerEntry{
		Kind: "thinking",
		Text: "First line of thought\nSecond line of thought",
	}
	r := &readerState{rawMode: false}
	lines := r.entryLines(entry, 80)
	full := strings.Join(lines, "\n")
	clean := sanitizeANSI(full)

	if !strings.Contains(clean, "Thought Process:") || !strings.Contains(clean, "First line of thought") || !strings.Contains(clean, "Second line of thought") {
		t.Fatalf("reader thinking view missing expected content:\n%s", clean)
	}
}

func TestReasoningStreamsInlineThenCollapses(t *testing.T) {
	m := astraModel(t, 80, 24)
	item := historyCell{kind: "thinking", toolID: "live", status: "streaming", text: "first fragment"}
	for _, render := range []func(*historyCell, int) string{m.renderLogItem, renderItemWidth} {
		if got := stripANSI(render(&item, 40)); !strings.Contains(got, "first fragment") {
			t.Fatalf("live reasoning hidden: %s", got)
		}
		item.text += "\nsecond fragment"
		if got := stripANSI(render(&item, 40)); !strings.Contains(got, "second fragment") {
			t.Fatalf("increment missing: %s", got)
		}
		item.status = "completed"
		if got := stripANSI(render(&item, 40)); strings.Contains(got, "fragment") || !strings.Contains(got, "Thought") {
			t.Fatalf("completed reasoning did not collapse: %s", got)
		}
		item.status = "streaming"
	}
	item.status = "completed"
	m.items = []historyCell{item}
	m.layout()
	m.toggleToolExpanded("live")
	if !strings.Contains(m.renderDetailPopover(), "second fragment") {
		t.Fatal("collapse lost reasoning contents")
	}
}
