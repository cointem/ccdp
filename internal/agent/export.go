package agent

import (
	"fmt"
	"strings"
	"time"

	"ccdp/internal/messages"
)

// History returns a copy of the conversation history.
func (a *Agent) History() []messages.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return messages.Clone(a.historyLocked())
}

// ExportMarkdown renders the session as a markdown transcript (Codex /export).
func (a *Agent) ExportMarkdown() string {
	a.mu.Lock()
	hist := messages.Clone(a.historyLocked())
	model, ws := a.services.cfg.Model, a.services.cfg.Workspace
	a.mu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "# ccdp session %s\n\n", a.conversation.sessionID)
	fmt.Fprintf(&sb, "- Model: %s\n- Workspace: `%s`\n- Exported: %s\n\n---\n\n",
		model, ws, time.Now().Format("2006-01-02 15:04:05"))

	for _, m := range hist {
		switch m.Role {
		case messages.RoleUser:
			fmt.Fprintf(&sb, "## User\n\n%s\n\n", m.Content)
		case messages.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n", m.Content)
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&sb, "\n_→ %s(%s)_\n", tc.Name, strings.TrimSpace(messages.MarshalArguments(tc.Arguments)))
				}
				sb.WriteString("\n")
			} else {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n\n", m.Content)
			}
		case messages.RoleTool:
			fmt.Fprintf(&sb, "### Tool result\n\n```text\n%s\n```\n\n", m.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}
