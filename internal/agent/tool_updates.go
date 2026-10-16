package agent

import (
	"fmt"

	"ccdp/internal/messages"
)

// recordStepTools runs on the owner, after the executable lease is frozen. The
// durable delta and the advertised definitions therefore describe that lease.
func (a *Agent) recordStepTools(step *stepRuntime) error {
	definitions := step.tools
	if step.plan {
		definitions = filterReadOnlyTools(definitions)
	}
	current := make([]messages.ToolDefinition, 0, len(definitions))
	for _, tool := range definitions {
		current = append(current, messages.ToolDefinition{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	previous, known := messages.CurrentTools(step.history)
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
