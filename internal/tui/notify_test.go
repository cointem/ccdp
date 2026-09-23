package tui

import (
	"strings"
	"testing"
)

func TestSanitizeNotificationStripsControls(t *testing.T) {
	// A crafted tool name must not be able to close the OSC string early and
	// inject a second escape sequence into the terminal.
	in := "rm -rf\x1b]0;evil\a\n/root\x00"
	got := sanitizeNotification(in)
	for _, bad := range []string{"\x1b", "\a", "\n", "\r", "\x00"} {
		if strings.Contains(got, bad) {
			t.Fatalf("sanitizeNotification kept control byte %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "rm -rf") || !strings.Contains(got, "evil") {
		t.Fatalf("sanitizeNotification dropped visible content: %q", got)
	}
}

func TestSanitizeNotificationTruncates(t *testing.T) {
	got := sanitizeNotification(strings.Repeat("x", 400))
	if len(got) > 120 {
		t.Fatalf("notification body not bounded: len=%d", len(got))
	}
}

func TestNotificationSequencePerTerminal(t *testing.T) {
	t.Run("ghostty uses osc9", func(t *testing.T) {
		t.Setenv("GHOSTTY_RESOURCES_DIR", "/some/path")
		t.Setenv("WEZTERM_EXECUTABLE", "")
		seq, ok := notificationSequence("ccdp", "done")
		if !ok || !strings.HasPrefix(seq, "\x1b]9;") || !strings.HasSuffix(seq, "\a") {
			t.Fatalf("ghostty sequence = %q", seq)
		}
	})
	t.Run("wezterm uses osc777", func(t *testing.T) {
		t.Setenv("GHOSTTY_RESOURCES_DIR", "")
		t.Setenv("WEZTERM_EXECUTABLE", "/usr/bin/wezterm")
		seq, ok := notificationSequence("ccdp", "done")
		if !ok || !strings.HasPrefix(seq, "\x1b]777;notify;") {
			t.Fatalf("wezterm sequence = %q", seq)
		}
	})
	t.Run("unknown terminal falls back to bell", func(t *testing.T) {
		t.Setenv("GHOSTTY_RESOURCES_DIR", "")
		t.Setenv("ITERM_SESSION_ID", "")
		t.Setenv("WEZTERM_EXECUTABLE", "")
		t.Setenv("WEZTERM_PANE", "")
		seq, _ := notificationSequence("ccdp", "done")
		if seq != "\a" {
			t.Fatalf("fallback sequence = %q, want bell", seq)
		}
	})
}
