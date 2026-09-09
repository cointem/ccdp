package tui

import (
	"ccdp/internal/agent"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Custom slash commands (Claude Code's .claude/commands feature): markdown
// files in ~/.ccdp/commands/<name>.md (user scope) or <workspace>/.ccdp/commands
// (project scope, wins on name collision). Running /name sends the file body
// as a user message with $ARGUMENTS replaced by the command's arguments.

type customCommand struct {
	name   string
	path   string
	source string // "user" | "project"
}

// loadCustomCommands discovers command files from both scopes.
func loadCustomCommands(workspace string) []customCommand {
	home, _ := os.UserHomeDir()
	scopes := []struct{ dir, source string }{
		{filepath.Join(home, ".ccdp", "commands"), "user"},
		{filepath.Join(workspace, ".ccdp", "commands"), "project"},
	}
	byName := map[string]customCommand{}
	for _, sc := range scopes {
		entries, err := os.ReadDir(sc.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if name == "" {
				continue
			}
			// Project scope (loaded later) overrides user scope.
			byName[name] = customCommand{
				name:   name,
				path:   filepath.Join(sc.dir, e.Name()),
				source: sc.source,
			}
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]customCommand, 0, len(byName))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}

// customCommandNames returns just the names (for autocomplete).
func (m *Model) customCommandNames() []string {
	names := make([]string, 0, len(m.customCmds))
	for _, c := range m.customCmds {
		names = append(names, c.name)
	}
	return names
}

// findCustomCommand looks up a command by name.
func (m *Model) findCustomCommand(name string) *customCommand {
	for i := range m.customCmds {
		if m.customCmds[i].name == name {
			return &m.customCmds[i]
		}
	}
	return nil
}

// runCustomCommand expands the command file and sends it as a user message.
func (m *Model) runCustomCommand(cmd *customCommand, args []string) {
	data, err := os.ReadFile(cmd.path)
	if err != nil {
		m.pushLog("error", "/"+cmd.name+": "+err.Error())
		return
	}
	body := string(data)
	argText := strings.Join(args, " ")
	body = strings.ReplaceAll(body, "$ARGUMENTS", argText)
	// "$1"–"$9" pick positional arguments (1-based, Claude Code semantics);
	// out-of-range indices expand to the empty string.
	for i := 1; i <= 9; i++ {
		val := ""
		if i <= len(args) {
			val = args[i-1]
		}
		body = strings.ReplaceAll(body, "$"+fmt.Sprint(i), val)
	}
	header := fmt.Sprintf("(custom command /%s from %s scope)", cmd.name, cmd.source)
	m.ctrl <- agent.Control{Type: agent.ControlUserMessage, Text: header + "\n\n" + strings.TrimSpace(body)}
}
