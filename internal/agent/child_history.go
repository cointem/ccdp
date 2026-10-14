package agent

import (
	"ccdp/internal/messages"
)

// cloneForkHistory copies the longest complete prefix. A tool round belongs
// to its assistant message and is complete only after every call has exactly
// one matching result. Never synthesize results for a parent's in-flight tools
// or discard earlier completed rounds when taking a child snapshot.
func cloneForkHistory(history []messages.Message) []messages.Message {
	boundary := 0
	for i := 0; i < len(history); {
		m := history[i]
		if m.Role == messages.RoleTool {
			break // An orphan result is not a safe fork boundary.
		}
		if len(m.ToolCalls) == 0 {
			i++
			boundary = i
			continue
		}
		if m.Role != messages.RoleAssistant {
			break
		}
		pending := make(map[string]bool, len(m.ToolCalls))
		valid := true
		for _, call := range m.ToolCalls {
			if call.ID == "" || pending[call.ID] {
				valid = false
				break
			}
			pending[call.ID] = true
		}
		if !valid {
			break
		}
		i++
		for i < len(history) && history[i].Role == messages.RoleTool {
			id := history[i].ToolCallID
			if !pending[id] {
				valid = false
				break
			}
			delete(pending, id)
			i++
		}
		if !valid || len(pending) != 0 {
			break
		}
		boundary = i
	}
	return messages.Clone(history[:boundary])
}
