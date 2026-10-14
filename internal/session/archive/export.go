package archive

import (
	"fmt"
	"strings"
	"time"

	"ccdp/internal/messages"
)

// Markdown renders a replayed snapshot without constructing an
// Agent, opening providers/MCP, acquiring a writer lock, or running hooks.
// It is used by the CLI's read-only replay mode.
func Markdown(snapshot *Snapshot) string {
	if snapshot == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# ccdp session %s\n\n", snapshot.ID)
	fmt.Fprintf(&sb, "- Model: %s\n- Workspace: `%s`\n- Exported: %s\n\n---\n\n",
		snapshot.Model, snapshot.Workspace, time.Now().Format("2006-01-02 15:04:05"))
	for _, message := range snapshot.History {
		if message.InternalSource != "" {
			fmt.Fprintf(&sb, "## Internal (%s)\n\n%s\n\n", message.InternalSource, message.Content)
			continue
		}
		switch message.Role {
		case messages.RoleUser:
			fmt.Fprintf(&sb, "## User\n\n%s\n\n", message.Content)
		case messages.RoleAssistant:
			if len(message.ToolCalls) > 0 {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n", message.Content)
				for _, call := range message.ToolCalls {
					fmt.Fprintf(&sb, "\n_→ %s(%s)_\n", call.Name, strings.TrimSpace(messages.MarshalArguments(call.Arguments)))
				}
				sb.WriteString("\n")
			} else {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n\n", message.Content)
			}
		case messages.RoleTool:
			fmt.Fprintf(&sb, "### Tool result\n\n```text\n%s\n```\n\n", message.Content)
		case messages.RoleSystem:
			fmt.Fprintf(&sb, "## System\n\n%s\n\n", message.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}
