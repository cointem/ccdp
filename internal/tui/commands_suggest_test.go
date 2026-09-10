package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/agent"
)

// sugModel builds a bare Model with just enough state for the suggestion logic.
func sugModel() *Model {
	// Production textarea construction so key bindings match the real TUI.
	ta := newTextarea()
	// Buffered control channel so key handlers that notify the agent
	// (e.g. ctrl+c interrupt) never block in tests.
	ctrl := make(chan agent.Control, 8)
	return &Model{textarea: ta, ctrl: ctrl}
}

func typeKey(m *Model, s string) {
	m.textarea, _ = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	m.refreshCmdSuggest()
}

func TestCommandNamesParsed(t *testing.T) {
	if len(commandNames) < 20 {
		t.Errorf("expected a substantial command list, got %d", len(commandNames))
	}
	for _, want := range []string{"help", "clear", "memory", "github", "pr-comments", "commit-push-pr", "quit", "exit", "config", "permissions"} {
		found := false
		for _, n := range commandNames {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("command %q missing from commandNames", want)
		}
	}
	// No stray punctuation from usage lines like "/quit, /exit".
	for _, n := range commandNames {
		if strings.ContainsAny(n, ",|/") {
			t.Errorf("command name has junk punctuation: %q", n)
		}
	}
}

func TestRefreshCmdSuggest(t *testing.T) {
	m := sugModel()

	// Plain text: no suggestions.
	typeKey(m, "fix the bug")
	if len(m.cmdSug) != 0 {
		t.Errorf("plain text opened suggestions: %v", m.cmdSug)
	}

	// "/" alone: suggestions are capped at maxCmdSuggestions entries; the
	// hidden remainder is surfaced via cmdSugMore (the "+N more" hint).
	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != maxCmdSuggestions {
		t.Errorf("'/' should suggest %d commands, got %d", maxCmdSuggestions, len(m.cmdSug))
	}
	if m.cmdSugMore != len(commandNames)-maxCmdSuggestions {
		t.Errorf("expected %d hidden matches, got %d", len(commandNames)-maxCmdSuggestions, m.cmdSugMore)
	}
	if m.cmdSugIdx != 0 {
		t.Errorf("selection should start at 0, got %d", m.cmdSugIdx)
	}

	// "/mem" filters to the memory command (plus none other).
	m.textarea.SetValue("/mem")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 1 || m.cmdSug[0] != "memory" {
		t.Errorf("'/mem' should suggest [memory], got %v", m.cmdSug)
	}

	// Exact full command: popup closes.
	m.textarea.SetValue("/memory")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("exact command should close the popup: %v", m.cmdSug)
	}

	// Arguments after a space: popup closed.
	m.textarea.SetValue("/git status --short")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("args after space should close the popup: %v", m.cmdSug)
	}

	// Unknown prefix: closed.
	m.textarea.SetValue("/zzz")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("unknown prefix should close the popup: %v", m.cmdSug)
	}
}

func TestCmdSuggestKeys(t *testing.T) {
	m := sugModel()

	// Open suggestions with "/g" → git, github.
	typeKey(m, "/g")
	if len(m.cmdSug) != 2 || m.cmdSug[0] != "git" || m.cmdSug[1] != "github" {
		t.Fatalf("'/g' should suggest [git github], got %v", m.cmdSug)
	}

	// Down moves selection, wraps.
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cmdSugIdx != 1 {
		t.Errorf("down should select index 1, got %d", m.cmdSugIdx)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cmdSugIdx != 0 {
		t.Errorf("down should wrap to index 0, got %d", m.cmdSugIdx)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.cmdSugIdx != 1 {
		t.Errorf("up should wrap to last index, got %d", m.cmdSugIdx)
	}

	// Tab accepts the highlighted command into the input.
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.textarea.Value(); got != "/github" {
		t.Errorf("tab should fill '/github', got %q", got)
	}
	if len(m.cmdSug) != 0 {
		t.Errorf("popup should close after accept, got %v", m.cmdSug)
	}

	// Typing a partial then enter accepts (does not submit a prefix).
	m.textarea.SetValue("")
	m.refreshCmdSuggest()
	typeKey(m, "/mem")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.textarea.Value(); got != "/memory" {
		t.Errorf("enter on a prefix should fill '/memory', got %q", got)
	}
}

func TestCtrlCTwoStageQuit(t *testing.T) {
	m := sugModel()
	cc := tea.KeyMsg{Type: tea.KeyCtrlC}

	// Busy: interrupt, no quit.
	m.busy = true
	if _, cmd := m.handleKey(cc); cmd != nil {
		t.Error("ctrl+c while busy should not quit")
	}
	m.busy = false

	// Idle: first press arms, second quits.
	if _, cmd := m.handleKey(cc); cmd != nil {
		t.Error("first ctrl+c when idle should not quit")
	}
	if !m.quitArmed {
		t.Error("first ctrl+c should arm the quit")
	}
	if _, cmd := m.handleKey(cc); cmd == nil {
		t.Error("second ctrl+c should quit")
	}

	// Any other key disarms.
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.quitArmed {
		t.Error("typing should disarm the quit")
	}
	if _, cmd := m.handleKey(cc); cmd != nil {
		t.Error("disarmed ctrl+c must not quit on first press")
	}
}

func TestScrollKeysReachViewport(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 3)
	m.baseVpH = 3
	// Fill with more content than fits and pin to the bottom (app default).
	m.viewport.SetContent("l1\nl2\nl3\nl4\nl5\nl6")
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset
	if bottom == 0 {
		t.Fatal("content should overflow the 3-line viewport")
	}

	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp}); m.viewport.YOffset >= bottom {
		t.Error("pgup should scroll the transcript up")
	}
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown}); m.viewport.YOffset != bottom {
		t.Error("pgdown should scroll back to the bottom")
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	m := sugModel()
	keys := m.textarea.KeyMap.InsertNewline.Keys()
	foundAlt, foundCtrlJ := false, false
	for _, k := range keys {
		switch k {
		case "alt+enter":
			foundAlt = true
		case "ctrl+j":
			foundCtrlJ = true
		}
	}
	if !foundAlt || !foundCtrlJ {
		t.Errorf("InsertNewline must bind alt+enter and ctrl+j, got %v", keys)
	}

	// Typing a line then alt+enter keeps the text and opens a new line.
	typeKey(m, "line one")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !strings.HasPrefix(m.textarea.Value(), "line one") {
		t.Errorf("alt+enter lost the text: %q", m.textarea.Value())
	}
	if !strings.Contains(m.textarea.Value(), "\n") {
		t.Errorf("alt+enter should insert a newline, got %q", m.textarea.Value())
	}
}

func TestQuitCommandReturnsQuitMsg(t *testing.T) {
	m := sugModel()
	for _, input := range []string{"/quit", "/exit"} {
		_, cmd := m.runCommand(input)
		if cmd == nil {
			t.Errorf("%s should return a quit command", input)
			continue
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Errorf("%s should produce a tea.QuitMsg, got %T", input, msg)
		}
	}
}

func TestCmdSuggestRendersMoreHint(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if m.cmdSugMore == 0 {
		t.Skip("no hidden matches to hint about")
	}
	out := m.renderCmdSuggest()
	if !strings.Contains(out, fmt.Sprintf("… +%d more", m.cmdSugMore)) {
		t.Errorf("expected a '+N more' hint line in the popup: %q", out)
	}
	// The hint is not an entry: len(cmdSug) downs from index 0 must wrap
	// straight back to 0 (an extra selectable line would land on index 8).
	for i := 0; i < len(m.cmdSug); i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.cmdSugIdx != 0 {
		t.Errorf("down should wrap within the %d entries, got index %d", len(m.cmdSug), m.cmdSugIdx)
	}
}

func TestRunDiffAndRunGitAreAsync(t *testing.T) {
	m := sugModel()

	if cmd := m.runDiff(nil); cmd == nil {
		t.Error("runDiff should return a tea.Cmd")
	} else if msg, ok := cmd().(gitResultMsg); !ok {
		t.Errorf("runDiff cmd should yield gitResultMsg, got %T", msg)
	}

	if cmd := m.runGit([]string{"status", "--short"}); cmd == nil {
		t.Error("runGit should return a tea.Cmd")
	} else if msg, ok := cmd().(gitResultMsg); !ok {
		t.Errorf("runGit cmd should yield gitResultMsg, got %T", msg)
	}
}

func TestPopupReservesViewportSpace(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 20)
	m.baseVpH = 20

	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if len(m.cmdSug) == 0 {
		t.Fatal("popup should open for '/'")
	}
	want := 20 - len(m.cmdSug) - 3
	if m.cmdSugMore > 0 {
		want-- // the "+N more" hint line
	}
	if want < 3 {
		want = 3
	}
	if m.viewport.Height != want {
		t.Errorf("viewport height = %d, want %d (popup %d entries + hint)", m.viewport.Height, want, len(m.cmdSug))
	}

	m.closeCmdSuggest()
	if m.viewport.Height != 20 {
		t.Errorf("closing the popup should restore height 20, got %d", m.viewport.Height)
	}
}
