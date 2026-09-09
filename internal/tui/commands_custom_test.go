package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/agent"
)

func TestLoadCustomCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()

	write := func(dir, name, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A user command and a project command with the same name (project wins).
	write(filepath.Join(home, ".ccdp", "commands"), "review.md", "Review $ARGUMENTS carefully.")
	write(filepath.Join(ws, ".ccdp", "commands"), "review.md", "PROJECT review $ARGUMENTS.")
	write(filepath.Join(ws, ".ccdp", "commands"), "deploy.md", "Deploy to $1 with $2.")
	// Non-md files are ignored.
	write(filepath.Join(ws, ".ccdp", "commands"), "notes.txt", "not a command")

	cmds := loadCustomCommands(ws)
	got := map[string]customCommand{}
	for _, c := range cmds {
		got[c.name] = c
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d: %v", len(cmds), cmds)
	}
	if got["review"].source != "project" {
		t.Errorf("project scope should override user scope: %+v", got["review"])
	}
	if _, ok := got["notes"]; ok {
		t.Error("non-markdown file must not become a command")
	}

	// Expansion: $ARGUMENTS and positional args.
	ctrl := make(chan agent.Control, 4)
	m := &Model{customCmds: cmds, ctrl: ctrl}
	cc := m.findCustomCommand("review")
	if cc == nil {
		t.Fatal("review command not found")
	}
	m.runCustomCommand(cc, []string{"auth.go"})
	select {
	case c := <-ctrl:
		if c.Type != agent.ControlUserMessage {
			t.Fatalf("expected user message control, got %v", c.Type)
		}
		if !strings.Contains(c.Text, "PROJECT review auth.go.") {
			t.Errorf("$ARGUMENTS not expanded: %q", c.Text)
		}
		if !strings.Contains(c.Text, "(custom command /review from project scope)") {
			t.Errorf("missing provenance header: %q", c.Text)
		}
	default:
		t.Fatal("no control message sent")
	}

	// Positional args: $1/$2.
	dep := m.findCustomCommand("deploy")
	m.runCustomCommand(dep, []string{"staging", "v2"})
	c := <-ctrl
	if !strings.Contains(c.Text, "Deploy to staging with v2.") {
		t.Errorf("positional args not expanded: %q", c.Text)
	}
}

func TestAutocompleteIncludesCustomCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	dir := filepath.Join(ws, ".ccdp", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("deploy"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := sugModel()
	m.customCmds = loadCustomCommands(ws)

	m.textarea.SetValue("/de")
	m.refreshCmdSuggest()
	found := false
	for _, s := range m.cmdSug {
		if s == "deploy" {
			found = true
		}
	}
	if !found {
		t.Errorf("custom command missing from autocomplete: %v", m.cmdSug)
	}
}
