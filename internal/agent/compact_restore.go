package agent

import (
	"fmt"
	"sort"
	"strings"

	"ccdp/internal/messages"
)

// restoreContextSnippet assembles a short "session context to restore" block
// appended to the compaction summary: discovered deferred tools, the active
// task list, and up to five files recently touched in the kept tail.
func (a *Agent) restoreContextSnippet(keptTail []messages.Message) string {
	var sb strings.Builder

	// Discovered deferred tools stay available after compaction.
	var disc []string
	for n := range a.discovery.deferTools {
		if a.isDiscovered(n) {
			disc = append(disc, n)
		}
	}
	if len(disc) > 0 {
		sort.Strings(disc)
		fmt.Fprintf(&sb, "Discovered tools available to you: %s\n", strings.Join(disc, ", "))
	}

	// Active task list comes from the committed session projection.
	if sec := a.todoSection(); sec != "" {
		sb.WriteString(sec)
		sb.WriteString("\n")
	}

	// Recently touched files, from tool arguments in the kept tail.
	seen := map[string]bool{}
	var files []string
	for _, m := range keptTail {
		for _, tc := range m.ToolCalls {
			for _, key := range []string{"file_path", "path", "file"} {
				if p, ok := tc.Arguments[key].(string); ok && p != "" && !seen[p] {
					seen[p] = true
					files = append(files, p)
				}
			}
		}
	}
	if len(files) > 5 {
		files = files[:5]
	}
	if len(files) > 0 {
		fmt.Fprintf(&sb, "Recently touched files: %s\n", strings.Join(files, ", "))
	}

	return strings.TrimRight(sb.String(), "\n")
}
