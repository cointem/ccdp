package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

const maxCustomCommandBytes = 1 << 20

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
			if !validCustomCommandName(name) {
				continue
			}
			name = strings.ToLower(name)
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

func validCustomCommandName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\t\r\n ")
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
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range m.customCmds {
		if m.customCmds[i].name == name {
			return &m.customCmds[i]
		}
	}
	return nil
}

// runCustomCommand expands the command file and sends it as a user message.
func (m *Model) runCustomCommand(cmd *customCommand, args []string) tea.Cmd {
	if cmd == nil {
		return nil
	}
	command := *cmd
	args = append([]string(nil), args...)
	session := m.sessionID
	generation := m.reportGeneration
	return func() tea.Msg {
		data, err := readCustomCommand(command.path)
		return customCommandLoadedMsg{command: command, args: args, session: session, generation: generation, data: data, err: err}
	}
}

func expandCustomCommand(cmd customCommand, args []string, data []byte) string {
	body := string(data)
	argText := strings.Join(args, " ")
	// Use one non-recursive replacement pass. Sequential ReplaceAll calls can
	// reinterpret a literal "$1" or "$ARGUMENTS" supplied inside an argument
	// value as another placeholder, changing the user's input.
	replacements := []string{"$ARGUMENTS", argText}
	// "$1"–"$9" pick positional arguments (1-based, Claude Code semantics);
	// out-of-range indices expand to the empty string. strings.Replacer emits
	// replacement text without scanning it again.
	for i := 1; i <= 9; i++ {
		val := ""
		if i <= len(args) {
			val = args[i-1]
		}
		replacements = append(replacements, "$"+fmt.Sprint(i), val)
	}
	body = strings.NewReplacer(replacements...).Replace(body)
	header := fmt.Sprintf("(custom command /%s from %s scope)", cmd.name, cmd.source)
	return header + "\n\n" + strings.TrimSpace(body)
}

func readCustomCommand(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("command file is not a regular file")
	}
	if info.Size() > maxCustomCommandBytes {
		return nil, fmt.Errorf("command file exceeds %d bytes", maxCustomCommandBytes)
	}
	// O_NONBLOCK prevents a path swapped to a FIFO after the admission stat
	// from wedging the Bubble Tea worker. Fstat closes that race before read.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("command file is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCustomCommandBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCustomCommandBytes {
		return nil, fmt.Errorf("command file exceeds %d bytes", maxCustomCommandBytes)
	}
	return data, nil
}
