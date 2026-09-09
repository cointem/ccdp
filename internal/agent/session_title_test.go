package agent

import (
	"ccdp/internal/messages"
	"strings"
	"testing"
)

func TestSessionTitle(t *testing.T) {
	hist := []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		{Role: messages.RoleAssistant, Content: "answer"},
		{Role: messages.RoleUser, Content: "fix the\n  bug in   parser.go please"},
	}
	got := sessionTitle(hist)
	want := "fix the bug in parser.go please"
	if got != want {
		t.Errorf("sessionTitle = %q, want %q", got, want)
	}

	// Long titles are capped (60 runes + ellipsis).
	long := strings.Repeat("x", 100)
	if got := sessionTitle([]messages.Message{{Role: messages.RoleUser, Content: long}}); !strings.HasSuffix(got, "…") || len([]rune(got)) != 61 {
		t.Errorf("long title not capped: %q (%d runes)", got, len([]rune(got)))
	}

	// No user message → empty title.
	if got := sessionTitle([]messages.Message{{Role: messages.RoleAssistant, Content: "hi"}}); got != "" {
		t.Errorf("expected empty title, got %q", got)
	}
}

func TestSessionTitleEmptyHistory(t *testing.T) {
	// Empty history snapshots tolerate a missing title.
	snap := SessionSnapshot{ID: "x", History: nil}
	if snap.Title != "" {
		t.Errorf("expected empty title, got %q", snap.Title)
	}
}
