package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"ccdp/internal/tools"
)

// ToolSearch discovers deferred tools by name or keywords (the tool_search /
// defer_loading idea from Codex and Claude Code). Tools with potentially large
// schemas (MCP servers, config custom tools) are not injected inline; the
// model calls ToolSearch to fetch their schemas, after which they become
// available for the rest of the session.
type toolSearchTool struct {
	ag *Agent
}

func (t *toolSearchTool) Name() string { return "ToolSearch" }

func (t *toolSearchTool) Description() string {
	return `Discover a tool whose schema is not shown inline. Call this with the exact
tool name (preferred) or a few keywords. Returns the tool's description and
JSON schema so you can call it. Discovered tools stay available for the rest
of the session.`
}

func (t *toolSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The exact tool name to load.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Keywords to match against tool names and descriptions.",
			},
		},
		"oneOf": []any{
			map[string]any{"required": []string{"name"}},
			map[string]any{"required": []string{"query"}},
		},
	}
}

func (t *toolSearchTool) Run(ctx *tools.Context) (string, error) {
	name := strings.TrimSpace(tools.StringArg(ctx.Args, "name", ""))
	query := strings.ToLower(strings.TrimSpace(tools.StringArg(ctx.Args, "query", "")))

	if name == "" && query == "" {
		return "", fmt.Errorf("ToolSearch: name or query required")
	}
	lease := t.ag.services.registry.Acquire()
	defer lease.Close()
	var matches []string
	for _, n := range lease.Names() {
		if t.ag.children.childState != nil && t.ag.children.childState.allowed != nil && !t.ag.children.childState.allowed[n] {
			continue
		}
		if name != "" {
			if strings.EqualFold(n, name) {
				matches = []string{n}
				break
			}
			continue
		}
		definition, _ := lease.Definition(n)
		hay := strings.ToLower(n + " " + definition.Description)
		if strings.Contains(hay, query) {
			matches = append(matches, n)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("ToolSearch: no allowed tool matches %q", firstNonEmpty(name, query))
	}
	sort.Strings(matches)

	var sb strings.Builder
	if name != "" {
		fmt.Fprintf(&sb, "Found tool %q:\n\n", name)
	} else {
		fmt.Fprintf(&sb, "Tools matching %q:\n\n", query)
	}
	for i, n := range matches {
		definition, _ := lease.Definition(n)
		description := definition.Description
		if name == "" {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&sb, "%s: %s\n", n, truncateRunes(description, 240))
			continue
		}
		parameters := definition.Parameters
		schema, err := json.MarshalIndent(parameters, "  ", "  ")
		if err != nil {
			return "", fmt.Errorf("ToolSearch: encode %s schema: %w", n, err)
		}
		limit := 50 << 10
		if ctx.OutputLimit > 0 {
			limit = min(limit, ctx.OutputLimit)
		}
		if len(schema)+len(description)+sb.Len()+100 > limit {
			return "", fmt.Errorf("ToolSearch: complete schema exceeds output budget; tool not loaded")
		}
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "Name: %s\nDescription: %s\nSchema:\n  %s", n, description, schema)
		// Make the discovered tool available for the rest of the session.
		if err := t.ag.persistToolDiscovery(lease.Version(), n, description, parameters); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// markDiscovered records that a deferred tool was surfaced via ToolSearch.
func (a *Agent) markDiscovered(name string) {
	a.mu.Lock()
	a.discovery.discovered[name] = true
	a.mu.Unlock()
}

// isDiscovered reports whether a deferred tool has been surfaced.
func (a *Agent) isDiscovered(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.discovery.discovered[name]
}

// inlineToolNames returns the names of tools injected directly (all non-deferred).
func (a *Agent) inlineToolNames() []string {
	var names []string
	for _, n := range a.services.registry.Names() {
		if a.discovery.deferTools[n] && !a.isDiscovered(n) {
			continue
		}
		names = append(names, n)
	}
	return names
}

// toolSchemas returns the tool declarations to inject this request: all
// non-deferred tools plus any discovered deferred tools.
func (a *Agent) toolSchemas() []map[string]any {
	return a.services.registry.SchemasFiltered(func(name string) bool {
		if a.children.childState != nil && a.children.childState.allowed != nil && !a.children.childState.allowed[name] {
			return false
		}
		return !a.discovery.deferTools[name] || a.isDiscovered(name)
	})
}
