package tui

import (
	"os"
	"strings"
)

// notificationBody is the free text shown by a desktop notification. It is built
// from runtime data (tool names, plan text) that must be treated as untrusted, so
// it is always sanitized before being wrapped in an escape sequence.

// notifyUser sends a best-effort desktop/terminal notification so a user who
// tabbed away learns when a long turn finishes or when the agent needs a
// decision. It selects a protocol the running terminal is known to accept and
// falls back to the plain terminal bell. Set CCDP_NOTIFY=off to disable.
func (m *Model) notifyUser(title, body string) {
	if strings.EqualFold(os.Getenv("CCDP_NOTIFY"), "off") {
		return
	}
	seq, ok := notificationSequence(title, body)
	if !ok {
		return
	}
	if m.terminal != nil {
		_, _ = m.terminal.Write([]byte(seq))
	}
}

// notificationSequence returns the raw escape payload for the current terminal,
// or ok=false when notifications are intentionally suppressed here (e.g. a
// headless test stdout). Callers fall back to nothing in that case.
func notificationSequence(title, body string) (string, bool) {
	msg := sanitizeNotification(title + ": " + body)
	switch {
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "", os.Getenv("ITERM_SESSION_ID") != "":
		// Ghostty and iTerm2 render OSC 9 as a desktop notification.
		return "\x1b]9;" + msg + "\a", true
	case os.Getenv("WEZTERM_EXECUTABLE") != "", os.Getenv("WEZTERM_PANE") != "":
		// WezTerm uses the xterm OSC 777 notify convention.
		return "\x1b]777;notify;" + sanitizeNotification(title) + ";" + sanitizeNotification(body) + "\a", true
	default:
		// Every other terminal gets the audible bell, matching prior behaviour.
		return "\a", true
	}
}

// sanitizeNotification strips control bytes (ESC, BEL, NUL, CR/LF and other C0)
// so untrusted tool/plan text cannot terminate the sequence early or inject a
// second escape sequence into the terminal.
func sanitizeNotification(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\x1b' || r == '\a' || r == 0 || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r < 0x20:
			// Drop remaining C0 control characters.
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
