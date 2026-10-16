package llm

import "ccdp/internal/messages"

type toolUpdateMode uint8

const (
	toolUpdatesAdditions toolUpdateMode = iota
	toolUpdatesChanges
)

type toolProjection struct {
	tools   []ToolDef
	updates map[int]*messages.ToolUpdate
	native  bool
}

func toolDefinitions(defs []ToolDef) []messages.ToolDefinition {
	out := make([]messages.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		out = append(out, messages.ToolDefinition{Name: def.Function.Name, Description: def.Function.Description, Parameters: def.Function.Parameters})
	}
	return out
}

func toolDefs(defs []messages.ToolDefinition) []ToolDef {
	out := make([]ToolDef, 0, len(defs))
	for _, def := range defs {
		out = append(out, ToolDef{Type: "function", Function: FuncDef{Name: def.Name, Description: def.Description, Parameters: def.Parameters}})
	}
	return out
}

// projectTools never changes the executable set in req.Tools. Incomplete
// history or an inconsistent catalog uses the full set.
// Addition-only transports also fall back after any removal or redefinition.
func projectTools(req CompletionRequest, mode toolUpdateMode) toolProjection {
	fallback := toolProjection{tools: req.Tools}
	start := -1
	for i, message := range req.Messages {
		if message.Role == "system" && message.ToolUpdate != nil && message.ToolUpdate.Initial {
			start = i
		}
	}
	if start < 0 {
		return fallback
	}
	initial := req.Messages[start].ToolUpdate.Added
	current := messages.ApplyToolUpdate(nil, req.Messages[start].ToolUpdate)
	updates := make(map[int]*messages.ToolUpdate)
	for i := start + 1; i < len(req.Messages); i++ {
		message := req.Messages[i]
		if message.Role != "system" || message.ToolUpdate == nil {
			continue
		}
		update := message.ToolUpdate
		if mode == toolUpdatesAdditions {
			if len(update.Removed) > 0 {
				return fallback
			}
			for _, added := range update.Added {
				for _, prior := range current {
					if added.Name == prior.Name {
						return fallback
					}
				}
			}
		}
		current = messages.ApplyToolUpdate(current, update)
		updates[i] = update
	}
	if messages.DiffTools(current, toolDefinitions(req.Tools), true) != nil {
		return fallback
	}
	return toolProjection{tools: toolDefs(initial), updates: updates, native: true}
}

func responsesTools(defs []ToolDef) []responsesTool {
	out := make([]responsesTool, 0, len(defs))
	for _, def := range defs {
		out = append(out, responsesTool{Type: "function", Name: def.Function.Name, Description: def.Function.Description, Parameters: def.Function.Parameters})
	}
	return out
}
