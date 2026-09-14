package tui

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
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
	client := &recordingClient{snapshot: protocol.SessionView{SessionID: "test-session"}}
	m := &Model{customCmds: cmds, client: client, sessionID: "test-session"}
	cc := m.findCustomCommand("review")
	if cc == nil {
		t.Fatal("review command not found")
	}
	applyCustomCommandTeaCmd(t, m, m.runCustomCommand(cc, []string{"auth.go"}))
	if len(client.submits) != 1 {
		t.Fatalf("expected one protocol input, got %d", len(client.submits))
	}
	c := client.submits[0]
	if !strings.Contains(c.Input.Text, "PROJECT review auth.go.") {
		t.Errorf("$ARGUMENTS not expanded: %q", c.Input.Text)
	}
	if !strings.Contains(c.Input.Text, "(custom command /review from project scope)") {
		t.Errorf("missing provenance header: %q", c.Input.Text)
	}

	// Positional args: $1/$2.
	dep := m.findCustomCommand("deploy")
	applyCustomCommandTeaCmd(t, m, m.runCustomCommand(dep, []string{"staging", "v2"}))
	c = client.submits[len(client.submits)-1]
	if !strings.Contains(c.Input.Text, "Deploy to staging with v2.") {
		t.Errorf("positional args not expanded: %q", c.Input.Text)
	}
}

func TestExpandCustomCommandDoesNotReexpandArguments(t *testing.T) {
	cmd := customCommand{name: "echo", source: "project"}
	got := expandCustomCommand(cmd, []string{"$2", "literal $1"}, []byte(
		"all=$ARGUMENTS first=$1 second=$2"))
	want := "(custom command /echo from project scope)\n\nall=$2 literal $1 first=$2 second=literal $1"
	if got != want {
		t.Fatalf("non-recursive expansion = %q, want %q", got, want)
	}
}

func applyCustomCommandTeaCmd(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("custom command returned no read command")
	}
	model, next := m.Update(cmd())
	*m = modelValue(t, model)
	if next == nil {
		t.Fatal("custom command read did not schedule submit")
	}
	model, _ = m.Update(next())
	*m = modelValue(t, model)
}

func TestBoundedCustomCommandRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	started := time.Now()
	if _, err := readCustomCommand(path); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("readCustomCommand FIFO error = %v, want regular-file rejection", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO check blocked for %s", elapsed)
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
