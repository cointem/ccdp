package agent

import (
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/tools"
)

// newAgentWithCustomTool builds an agent with one config custom tool (deferred).
func newAgentWithCustomTool(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.Tools = []config.ToolSpec{
		{Name: "my-fmt", Description: "format files", Command: "gofmt"},
	}
	events := make(chan Event, 64)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestDeferredToolNotInjected(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	defer ag.Close()

	// The custom tool is deferred: absent from inline schemas.
	schemas := ag.toolSchemas()
	for _, s := range schemas {
		name := s["function"].(map[string]any)["name"].(string)
		if name == "my-fmt" {
			t.Fatal("deferred custom tool should not be injected before discovery")
		}
	}

	// ToolSearch itself is always available.
	foundSearch := false
	for _, s := range schemas {
		if s["function"].(map[string]any)["name"].(string) == "ToolSearch" {
			foundSearch = true
		}
	}
	if !foundSearch {
		t.Error("ToolSearch should always be injected")
	}

	// The actual frozen request advertises only the step's inline definitions;
	// the deferred custom tool must remain absent until discovery.
	step, err := ag.beginStepChecked()
	if err != nil {
		t.Fatalf("beginStepChecked: %v", err)
	}
	req, err := ag.buildRequestForStepChecked(step)
	releaseStepLease(step)
	ag.mu.Lock()
	ag.childStep = nil
	ag.mu.Unlock()
	if err != nil {
		t.Fatalf("buildRequestForStepChecked: %v", err)
	}
	sys, _ := req.Messages[0].Content.(string)
	if !strings.Contains(sys, "# Available tools") || !strings.Contains(sys, "ToolSearch") {
		t.Errorf("frozen availability section missing ToolSearch: %q", sys)
	}
	if strings.Contains(sys, "my-fmt") {
		t.Errorf("deferred tool leaked into frozen availability section: %q", sys)
	}

	// Simulate a discovery: mark it and re-check.
	ag.markDiscovered("my-fmt")
	found := false
	for _, s := range ag.toolSchemas() {
		if s["function"].(map[string]any)["name"].(string) == "my-fmt" {
			found = true
		}
	}
	if !found {
		t.Error("discovered deferred tool should be injected on the next request")
	}

	// A later step captures the updated discovery state and advertises the
	// custom schema in the wire request.
	step, err = ag.beginStepChecked()
	if err != nil {
		t.Fatalf("beginStepChecked after discovery: %v", err)
	}
	req, err = ag.buildRequestForStepChecked(step)
	releaseStepLease(step)
	ag.mu.Lock()
	ag.childStep = nil
	ag.mu.Unlock()
	if err != nil {
		t.Fatalf("buildRequestForStepChecked after discovery: %v", err)
	}
	sys, _ = req.Messages[0].Content.(string)
	if !strings.Contains(sys, "my-fmt") {
		t.Errorf("discovered tool missing from frozen availability section: %q", sys)
	}
}

func TestToolSearchRun(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	defer ag.Close()

	// Keyword search finds the custom tool by description.
	ts := &toolSearchTool{ag: ag}
	out, err := ts.Run(&tools.Context{
		WorkingDir: ag.cfg.Workspace,
		Args:       map[string]any{"query": "format files"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "my-fmt") || !strings.Contains(out, "Schema:") {
		t.Errorf("search output missing tool details: %.200s", out)
	}
	if !ag.isDiscovered("my-fmt") {
		t.Error("search should mark the tool discovered")
	}

	// Exact-name search also works.
	ag2 := newAgentWithCustomTool(t)
	defer ag2.Close()
	ts2 := &toolSearchTool{ag: ag2}
	out2, err := ts2.Run(&tools.Context{
		WorkingDir: ag2.cfg.Workspace,
		Args:       map[string]any{"name": "my-fmt"},
	})
	if err != nil || !strings.Contains(out2, "my-fmt") {
		t.Errorf("exact-name search failed: %v %q", err, out2)
	}

	// Unknown name errors.
	_, err = ts2.Run(&tools.Context{WorkingDir: ag2.cfg.Workspace, Args: map[string]any{"name": "nope"}})
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestBuildRequestSections(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	defer ag.Close()

	step, err := ag.beginStepChecked()
	if err != nil {
		t.Fatalf("beginStepChecked: %v", err)
	}
	req, err := ag.buildRequestForStepChecked(step)
	releaseStepLease(step)
	if err != nil {
		t.Fatalf("buildRequestForStepChecked: %v", err)
	}
	ag.mu.Lock()
	ag.childStep = nil
	ag.mu.Unlock()
	if len(req.Messages) == 0 {
		t.Fatal("no messages")
	}
	sys, _ := req.Messages[0].Content.(string)
	for _, want := range []string{"# Environment", "- Date:", "- Timezone:", "# Repository layout", "# Available tools", "# Project instructions"} {
		// Project instructions only present when an AGENTS.md exists.
		if want == "# Project instructions" {
			continue
		}
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
