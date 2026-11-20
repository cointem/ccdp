package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

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
	return `Load a tool by exact name or an unambiguous keyword match. Its schema
appears in the next tool catalog and stays available for this session.
Ambiguous queries return up to five candidates; choose an exact name.`
}

func (t *toolSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "The exact tool name to load.",
			},
			"query": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Describe the task or enter a tool name. Related matches are ranked; select a candidate by name to load it.",
			},
		},
		"oneOf": []any{
			map[string]any{"required": []string{"name"}},
			map[string]any{"required": []string{"query"}},
		},
		"additionalProperties": false,
	}
}

func (t *toolSearchTool) Execute(ctx *tools.Context, args map[string]any) (tools.Result, error) {
	ctx = ctx.WithArgs(args)
	text, err := func() (string, error) {
		name := strings.TrimSpace(tools.StringArg(ctx.Args, "name", ""))
		query := strings.ToLower(strings.TrimSpace(tools.StringArg(ctx.Args, "query", "")))

		if (name == "") == (query == "") {
			return "", fmt.Errorf("ToolSearch: provide exactly one of name or query")
		}
		lease := t.ag.services.registry.Acquire()
		keywords := toolSearchWords(query)
		type candidate struct {
			name            string
			coverage, score int
		}
		var matches []candidate
		exact := false
		for _, n := range lease.Names() {
			if t.ag.children.childState != nil && t.ag.children.childState.allowed != nil && !t.ag.children.childState.allowed[n] {
				continue
			}
			if strings.EqualFold(n, name) || strings.EqualFold(n, query) {
				matches = []candidate{{name: n}}
				exact = true
				break
			}
			if name != "" {
				continue
			}
			definition, _ := lease.Definition(n)
			if definition.ValidationError() != nil {
				continue
			}
			coverage, score := scoreToolSearch(definition, keywords)
			if score > 0 {
				matches = append(matches, candidate{n, coverage, score})
			}
		}
		if len(matches) == 0 {
			if name != "" {
				return fmt.Sprintf("No available tool named %q in this session.", name), nil
			}
			return fmt.Sprintf("No available tool matches %q in this session.", query), nil
		}
		sort.SliceStable(matches, func(i, j int) bool {
			if matches[i].coverage != matches[j].coverage {
				return matches[i].coverage > matches[j].coverage
			}
			return matches[i].score > matches[j].score
		})
		load := exact || matches[0].coverage == len(keywords) && (len(matches) == 1 || matches[1].coverage < len(keywords))
		if load {
			matches = matches[:1]
		} else {
			matches = matches[:min(5, len(matches))]
		}

		var sb strings.Builder
		if name != "" {
			fmt.Fprintf(&sb, "Found tool %q:\n\n", name)
		} else {
			fmt.Fprintf(&sb, "Tools matching %q:\n\n", query)
		}
		for _, m := range matches {
			n := m.name
			definition, _ := lease.Definition(n)
			if err := definition.ValidationError(); err != nil {
				return "", fmt.Errorf("ToolSearch: %s is unavailable because its schema is invalid: %w", n, err)
			}
			description := definition.Description
			if !load {
				fmt.Fprintf(&sb, "%s: %s\n", n, truncateRunes(description, 240))
				continue
			}
			parameters := definition.Parameters
			schema, err := json.Marshal(parameters)
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
			fmt.Fprintf(&sb, "Loaded %s: %s\nIts complete schema is available in the next tool catalog.", n, description)
			// Make the discovered tool available for the rest of the session.
			if err := t.ag.persistToolDiscovery(lease.Version(), n, description, parameters); err != nil {
				return "", err
			}
		}
		if !load {
			sb.WriteString("Use name to load a candidate.")
		}
		return sb.String(), nil
	}()
	return tools.Result{Text: text}, err
}

func toolSearchWords(s string) map[string]bool {
	words := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch word {
		case "a", "an", "the", "to", "for", "of", "and", "with", "in", "by":
		default:
			words[word] = true
		}
	}
	return words
}

// Require evidence in the name, primary purpose or input fields. Mentions in
// usage notes (e.g. Read explains Bash paths) are supporting evidence only.
func scoreToolSearch(def tools.Definition, query map[string]bool) (coverage, score int) {
	name := toolSearchWords(def.Name)
	purpose, _, _ := strings.Cut(def.Description, ".")
	primary := toolSearchWords(purpose)
	props, _ := def.Parameters["properties"].(map[string]any)
	for key := range props {
		if key != "requested_capabilities" {
			for word := range toolSearchWords(key) {
				primary[word] = true
			}
		}
	}
	description := toolSearchWords(def.Description)
	anchor := false
	for word := range query {
		switch {
		case name[word]:
			anchor = true
			score += 5
		case primary[word]:
			anchor = true
			score += 3
		case description[word]:
			score++
		default:
			continue
		}
		coverage++
	}
	if !anchor || coverage*2 < len(query) {
		return 0, 0
	}
	return coverage, score
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
