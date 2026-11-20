package agent

import (
	"fmt"

	"ccdp/internal/messages"
)

// recordStepTools runs on the owner, after the executable lease is frozen. The
// durable delta and the advertised definitions therefore describe that lease.
func (a *Agent) recordStepTools(step *stepRuntime) error {
	definitions := step.tools
	current := make([]messages.ToolDefinition, 0, len(definitions))
	for _, tool := range definitions {
		current = append(current, messages.ToolDefinition{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	previous, known := messages.CurrentTools(step.history)
	if child := a.children.childState; known && child != nil && child.allowed != nil {
		known = catalogWithinScope(step.history, child.allowed)
	}
	update := messages.DiffTools(previous, current, known)
	if update == nil {
		return nil
	}
	message := messages.Message{Role: messages.RoleSystem, InternalSource: "tool_catalog", ToolUpdate: update}
	if err := a.appendHistoryLockedForStep(fmt.Sprintf("step-%d", step.step), message); err != nil {
		return err
	}
	a.mu.Lock()
	step.history = messages.Clone(a.historyLocked())
	a.mu.Unlock()
	return nil
}

// Older desktop clients could reopen a delegated session as a root and record
// its wider catalog. A removal alone would still replay those declarations on
// incremental transports. Start a new baseline when the current one crosses
// the child's tool boundary, including tools later removed from that baseline.
func catalogWithinScope(history []messages.Message, allowed map[string]bool) bool {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != messages.RoleSystem || m.ToolUpdate == nil {
			continue
		}
		for _, def := range m.ToolUpdate.Added {
			if !allowed[def.Name] {
				return false
			}
		}
		if m.ToolUpdate.Initial {
			break
		}
	}
	return true
}
