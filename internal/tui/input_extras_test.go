package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func runeKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// ctrl+r opens reverse search; typing filters newest-first; Enter recalls the
// match into the composer without submitting; Esc cancels.
func TestHistorySearchRecallAndCancel(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.history = []string{"fix the footer", "add a test", "fix the header"}
	m.historyIdx = len(m.history)

	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.histSearch == nil {
		t.Fatal("ctrl+r did not open reverse search")
	}
	// Empty query previews the most recent entry.
	if got := m.historySearchSelected(); got != "fix the header" {
		t.Fatalf("default match = %q, want newest entry", got)
	}
	m.handleKey(runeKey("footer"))
	if got := m.historySearchSelected(); got != "fix the footer" {
		t.Fatalf("query 'footer' matched %q", got)
	}
	m.handleKey(runeKey("zzz"))
	if got := m.historySearchSelected(); got != "" {
		t.Fatalf("no-match query should select nothing, got %q", got)
	}
	if !strings.Contains(sanitizeANSI(m.renderHistorySearch()), "no match") {
		t.Fatalf("prompt should report no match:\n%s", sanitizeANSI(m.renderHistorySearch()))
	}
	// Backspace back to a real query, then Enter recalls it.
	for i := 0; i < 3; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.histSearch != nil {
		t.Fatal("Enter should close reverse search")
	}
	if got := m.textarea.Value(); got != "fix the footer" {
		t.Fatalf("recalled draft = %q, want 'fix the footer'", got)
	}

	// Esc cancels without touching the draft.
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.histSearch != nil {
		t.Fatal("Esc should cancel reverse search")
	}
	if got := m.textarea.Value(); got != "fix the footer" {
		t.Fatalf("cancel changed the draft: %q", got)
	}
}

// ctrl+r is a no-op when there is no history to search.
func TestHistorySearchNoHistory(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.history = nil
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.histSearch != nil {
		t.Fatal("reverse search opened with empty history")
	}
}

func TestTrailingToken(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"hello":           "hello",
		"explain @src/ma": "@src/ma",
		"a b\tc":          "c",
		"multi\n@tok":     "@tok",
		"trailing space ": "",
	}
	for in, want := range cases {
		if got := trailingToken(in); got != want {
			t.Errorf("trailingToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// @-completion lists one directory level, filters by prefix, sorts directories
// first, and drills into a selected directory.
func TestFileMentionCompletion(t *testing.T) {
	root := t.TempDir()
	mustMkdir := func(p string) { os.MkdirAll(filepath.Join(root, p), 0o755) }
	mustMkdir("internal/tui")
	mustMkdir("internal/agent")
	os.WriteFile(filepath.Join(root, "internal", "agent", "agent.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "internal", "notes.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644)

	m := astraModel(t, 80, 24)
	m.workspace = root

	// Typing "@internal/" lists that directory, directories first.
	m.textarea.SetValue("explain @internal/")
	m.textarea.CursorEnd()
	m.refreshFileMention()
	if m.fileMention == nil {
		t.Fatal("@internal/ did not open completion")
	}
	labels := make([]string, 0, len(m.fileMention.items))
	for _, it := range m.fileMention.items {
		labels = append(labels, it.label)
	}
	if strings.Join(labels, ",") != "agent/,tui/,notes.md" {
		t.Fatalf("unexpected listing order: %v", labels)
	}

	// Accepting the first (directory) candidate drills in.
	m.acceptFileMention()
	if got := m.textarea.Value(); got != "explain @internal/agent/" {
		t.Fatalf("drill-in value = %q", got)
	}
	if m.fileMention == nil {
		t.Fatal("directory accept should reopen completion for drill-in")
	}

	// A prefix filters the listing.
	m.textarea.SetValue("@READ")
	m.textarea.CursorEnd()
	m.refreshFileMention()
	if m.fileMention == nil || len(m.fileMention.items) != 1 || m.fileMention.items[0].label != "README.md" {
		t.Fatalf("prefix filter failed: %#v", m.fileMention)
	}
	m.acceptFileMention()
	if got := m.textarea.Value(); got != "@README.md" {
		t.Fatalf("file accept value = %q", got)
	}
	if m.fileMention != nil {
		t.Fatal("file accept should close completion")
	}
}

// A mention that resolves to nothing leaves the popup closed.
func TestFileMentionNoMatch(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.workspace = t.TempDir()
	m.textarea.SetValue("@does-not-exist")
	m.textarea.CursorEnd()
	m.refreshFileMention()
	if m.fileMention != nil {
		t.Fatal("popup opened for a non-existent path")
	}
}

// closeExternalEditor folds the edited file back into the composer, trims the
// trailing newline editors add, and removes the staging file.
func TestCloseExternalEditor(t *testing.T) {
	m := astraModel(t, 80, 24)
	f, err := os.CreateTemp("", "ccdp-draft-*.md")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.WriteString("line one\nline two\n")
	f.Close()

	m.closeExternalEditor(editorFinishedMsg{path: path})
	if got := m.textarea.Value(); got != "line one\nline two" {
		t.Fatalf("draft = %q, want trailing newline trimmed", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staging file was not removed: %v", err)
	}
}

// A failed editor exit keeps the existing draft and cleans up the temp file.
func TestCloseExternalEditorError(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.textarea.SetValue("keep me")
	f, _ := os.CreateTemp("", "ccdp-draft-*.md")
	path := f.Name()
	f.Close()
	m.closeExternalEditor(editorFinishedMsg{path: path, err: os.ErrPermission})
	if got := m.textarea.Value(); got != "keep me" {
		t.Fatalf("draft changed on editor error: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staging file was not removed on error: %v", err)
	}
}

// externalEditor honours $VISUAL over $EDITOR and splits extra arguments.
func TestExternalEditorResolution(t *testing.T) {
	t.Setenv("VISUAL", "code --wait")
	t.Setenv("EDITOR", "nano")
	name, args := externalEditor()
	if name != "code" || strings.Join(args, " ") != "--wait" {
		t.Fatalf("VISUAL should win: %q %v", name, args)
	}
	t.Setenv("VISUAL", "")
	name, args = externalEditor()
	if name != "nano" || len(args) != 0 {
		t.Fatalf("EDITOR fallback failed: %q %v", name, args)
	}
}
