package agent

import (
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

func TestWebSearchCatalogUsesConfiguredMCP(t *testing.T) {
	for _, installed := range []bool{true, false} {
		cfg := isolatedTestConfig(t, "search-catalog")
		if !installed {
			delete(cfg.MCPServers, mcp.ExaServer)
		}
		a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		catalog := a.services.registry.Acquire()
		if _, ok := catalog.Get("WebSearch"); ok {
			t.Fatal("local WebSearch appeared in the tool catalog")
		}
		if tool, ok := catalog.Get(mcp.ExaToolName()); ok != installed || ok && !mcp.IsExaSearch(tool) {
			t.Fatalf("Exa installed=%t, catalog entry=%v", installed, tool)
		}
		if _, ok := catalog.Get("WebFetch"); !ok {
			t.Fatal("missing URL reader")
		}
		if !installed {
			continue
		}
		hasExa := func() bool {
			for _, def := range a.toolDefsSnapshotFromLease(catalog) {
				if def.Function.Name == mcp.ExaToolName() {
					return true
				}
			}
			return false
		}
		if hasExa() {
			t.Fatal("Exa schema was sent before discovery")
		}
		search, _ := catalog.Get("ToolSearch")
		if _, err := executeToolText(search, &tools.Context{Args: map[string]any{"name": mcp.ExaToolName()}}); err != nil || !hasExa() {
			t.Fatalf("Exa schema missing after discovery: %v", err)
		}
	}
}

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
	ag, err := testNew(t, &cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestDeferredToolNotInjected(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	defer ag.Close()

	// The custom tool is deferred: absent from inline schemas.
	schemas := ag.toolDefsSnapshotFromLease(ag.services.registry.Acquire())
	for _, s := range schemas {
		name := s.Function.Name
		if name == "my-fmt" {
			t.Fatal("deferred custom tool should not be injected before discovery")
		}
	}

	// ToolSearch itself is always available.
	foundSearch := false
	for _, s := range schemas {
		if s.Function.Name == "ToolSearch" {
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
	ag.children.childStep = nil
	ag.mu.Unlock()
	if err != nil {
		t.Fatalf("buildRequestForStepChecked: %v", err)
	}
	sys, _ := req.Messages[0].Content.(string)
	if strings.Contains(sys, "# Available tools") {
		t.Fatal("mutable availability list must not be in base system")
	}
	searchDeclared := false
	for _, tool := range req.Tools {
		if tool.Function.Name == "ToolSearch" {
			searchDeclared = true
		}
	}
	if !searchDeclared {
		t.Fatal("ToolSearch schema missing")
	}
	if strings.Contains(sys, "my-fmt") {
		t.Errorf("deferred tool leaked into frozen availability section: %q", sys)
	}

	// Simulate a discovery: mark it and re-check.
	ag.markDiscovered("my-fmt")
	found := false
	for _, s := range ag.toolDefsSnapshotFromLease(ag.services.registry.Acquire()) {
		if s.Function.Name == "my-fmt" {
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
	ag.children.childStep = nil
	ag.mu.Unlock()
	if err != nil {
		t.Fatalf("buildRequestForStepChecked after discovery: %v", err)
	}
	sys, _ = req.Messages[0].Content.(string)
	if strings.Contains(sys, "my-fmt") {
		t.Fatal("discovery changed base system")
	}
	found = false
	for _, tool := range req.Tools {
		if tool.Function.Name == "my-fmt" {
			found = true
		}
	}
	if !found {
		t.Fatal("discovered tool schema missing")
	}
	var update *messages.ToolUpdate
	for _, m := range step.history {
		if m.ToolUpdate != nil {
			update = m.ToolUpdate
		}
	}
	if update == nil || len(update.Added) != 1 || update.Added[0].Name != "my-fmt" {
		t.Fatalf("missing discovery delta: %+v", update)
	}
}

func TestToolSearchRun(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	defer ag.Close()

	// Keyword search finds the custom tool by description.
	ts := &toolSearchTool{ag: ag}
	out, err := executeToolText(ts, &tools.Context{
		WorkingDir: ag.services.cfg.Workspace,
		Args:       map[string]any{"query": "format files"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "my-fmt") || strings.Contains(out, "Schema:") {
		t.Errorf("search output missing tool details: %.200s", out)
	}
	if !ag.isDiscovered("my-fmt") {
		t.Error("unambiguous keyword match must load the schema")
	}

	// Exact-name search also works.
	ag2 := newAgentWithCustomTool(t)
	defer ag2.Close()
	ts2 := &toolSearchTool{ag: ag2}
	out2, err := executeToolText(ts2, &tools.Context{
		WorkingDir: ag2.services.cfg.Workspace,
		Args:       map[string]any{"name": "my-fmt"},
	})
	if err != nil || !strings.Contains(out2, "my-fmt") {
		t.Errorf("exact-name search failed: %v %q", err, out2)
	}

	// An empty lookup is a successful search; it loads nothing.
	out, err = executeToolText(ts2, &tools.Context{WorkingDir: ag2.services.cfg.Workspace, Args: map[string]any{"name": "nope"}})
	if err != nil || !strings.Contains(out, "No available tool") || ag2.isDiscovered("nope") {
		t.Fatalf("unknown name: %q, %v", out, err)
	}
}

func TestExplorerDiscoversSearchByKeywordsWithinItsRole(t *testing.T) {
	cfg := isolatedTestConfig(t, "explorer")
	ag, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, Purpose: childPurposeTask, ReadOnlyWorkspace: true, AllowedTools: childReadOnlyTools(childPurposeTask)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	search := &toolSearchTool{ag: ag}
	for query, want := range map[string]string{
		"grep search file contents": "Loaded Rg:", "grep pattern text search": "Loaded Rg:",
		"grep search file contents regex": "Loaded Rg:", "RG": "Loaded Rg:",
		"grep search file contents bash shell": "Rg:", "run shell command bash": "No available tool matches",
	} {
		out, err := executeToolText(search, &tools.Context{WorkingDir: cfg.Workspace, Args: map[string]any{"query": query}})
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("%q returned %q, %v", query, out, err)
		}
	}
	if out, err := executeToolText(search, &tools.Context{Args: map[string]any{"name": "Bash"}}); err != nil || !strings.Contains(out, "No available tool") || ag.isDiscovered("Bash") {
		t.Fatalf("restricted tool lookup: %q, %v", out, err)
	}
}

func TestPartialToolMatchSuggestsWithoutLoading(t *testing.T) {
	ag := newAgentWithCustomTool(t)
	t.Cleanup(ag.Close)
	out, err := executeToolText(&toolSearchTool{ag: ag}, &tools.Context{Args: map[string]any{"query": "format files prettify"}})
	if err != nil || !strings.Contains(out, "my-fmt:") || ag.isDiscovered("my-fmt") {
		t.Fatalf("partial match: %q, %v", out, err)
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
	ag.children.childStep = nil
	ag.mu.Unlock()
	if len(req.Messages) == 0 {
		t.Fatal("no messages")
	}
	sys, _ := req.Messages[0].Content.(string)
	for _, want := range []string{"# Environment", "- Date:", "- Timezone:", "# Project instructions"} {
		// Project instructions only present when an AGENTS.md exists.
		if want == "# Project instructions" {
			continue
		}
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
