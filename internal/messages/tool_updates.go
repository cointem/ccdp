package messages

import (
	"encoding/json"
	"slices"

	"ccdp/internal/jsonvalue"
)

// ToolDefinition is a model-visible declaration, without executable capabilities.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolUpdate is a durable catalog delta. Initial starts a new catalog after a
// fresh session or a history rewrite that discarded the previous baseline.
type ToolUpdate struct {
	Initial bool             `json:"initial,omitempty"`
	Added   []ToolDefinition `json:"added,omitempty"`
	Removed []string         `json:"removed,omitempty"`
}

func CloneToolUpdate(update *ToolUpdate) *ToolUpdate {
	if update == nil {
		return nil
	}
	copy := *update
	copy.Removed = slices.Clone(update.Removed)
	copy.Added = slices.Clone(update.Added)
	for i := range copy.Added {
		copy.Added[i].Parameters = jsonvalue.Clone(copy.Added[i].Parameters)
	}
	return &copy
}

func ToolDefinitionsEqual(a, b ToolDefinition) bool {
	x, err := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err == nil && err2 == nil && string(x) == string(y)
}

// CurrentTools replays only a complete catalog; orphan deltas after compaction
// do not constitute a baseline.
func CurrentTools(history []Message) ([]ToolDefinition, bool) {
	var tools []ToolDefinition
	known := false
	for _, message := range history {
		if message.Role != RoleSystem || message.ToolUpdate == nil {
			continue
		}
		update := message.ToolUpdate
		if update.Initial {
			tools = nil
			known = true
		}
		if known {
			tools = ApplyToolUpdate(tools, update)
		}
	}
	return tools, known
}

func ApplyToolUpdate(tools []ToolDefinition, update *ToolUpdate) []ToolDefinition {
	if update.Initial {
		tools = nil
	}
	out := slices.Clone(tools)
	out = slices.DeleteFunc(out, func(tool ToolDefinition) bool { return slices.Contains(update.Removed, tool.Name) })
	for _, added := range update.Added {
		i := slices.IndexFunc(out, func(tool ToolDefinition) bool { return tool.Name == added.Name })
		if i < 0 {
			out = append(out, added)
		} else {
			out[i] = added
		}
	}
	return out
}

// DiffTools records a changed definition as removal plus addition. Ordering is
// retained across requests so equivalent catalogs do not churn the prefix.
func DiffTools(previous, current []ToolDefinition, known bool) *ToolUpdate {
	update := &ToolUpdate{Initial: !known}
	old := make(map[string]ToolDefinition, len(previous))
	wanted := make(map[string]ToolDefinition, len(current))
	for _, tool := range previous {
		old[tool.Name] = tool
	}
	for _, tool := range current {
		wanted[tool.Name] = tool
		prior, exists := old[tool.Name]
		if !known || !exists || !ToolDefinitionsEqual(prior, tool) {
			update.Added = append(update.Added, tool)
		}
	}
	if known {
		for _, tool := range previous {
			next, exists := wanted[tool.Name]
			if !exists || !ToolDefinitionsEqual(tool, next) {
				update.Removed = append(update.Removed, tool.Name)
			}
		}
	}
	if known && len(update.Added) == 0 && len(update.Removed) == 0 {
		return nil
	}
	return CloneToolUpdate(update)
}
