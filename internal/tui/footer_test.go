package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// A wide terminal should collapse the persistent state and the contextual hints
// onto a single space-between row, matching the calmer one-row chrome of the
// sibling CLIs. A narrow terminal keeps them stacked so the wrapped state and
// the context estimate both stay legible.
func TestFooterCollapsesToSingleRowWhenWide(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.layout()
	footer := m.renderFooter()
	if height := lipgloss.Height(footer); height != 1 {
		t.Fatalf("wide footer should be one row, got %d:\n%s", height, sanitizeANSI(footer))
	}
	if width := lipgloss.Width(footer); width > m.width {
		t.Fatalf("collapsed footer exceeds terminal: %d > %d", width, m.width)
	}
	plain := sanitizeANSI(footer)
	if !strings.Contains(plain, "deepseek-chat") || !strings.Contains(plain, "12.3k/200k") {
		t.Fatalf("collapsed footer lost the state triplet: %q", plain)
	}
	if !strings.Contains(plain, "enter send") {
		t.Fatalf("collapsed footer lost the hints: %q", plain)
	}
	// Right-aligned hints must not crowd the state: at least one space between.
	if strings.Contains(plain, "200kenter") {
		t.Fatalf("state and hints are not separated: %q", plain)
	}
}

func TestFooterStacksWhenNarrow(t *testing.T) {
	m := astraModel(t, 40, 16)
	m.layout()
	plain := sanitizeANSI(m.renderFooter())
	if !strings.Contains(plain, "12.3k/200k") || !strings.Contains(plain, "enter send") {
		t.Fatalf("narrow footer dropped state or hints:\n%s", plain)
	}
}

// Every footer row aligns with the composer's one-cell left padding rather than
// sitting flush-left, and carries no invisible full-width trailing padding.
func TestFooterRowsAreIndentedAndTrimmed(t *testing.T) {
	for _, width := range []int{24, 40, 80, 120} {
		m := astraModel(t, width, 24)
		m.layout()
		for _, line := range strings.Split(sanitizeANSI(m.renderFooter()), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, " ") {
				t.Fatalf("width %d: footer row is flush-left, want one-cell indent: %q", width, line)
			}
			if strings.HasSuffix(line, " ") {
				t.Fatalf("width %d: footer row kept trailing padding: %q", width, line)
			}
		}
	}
}

// A modal owns Enter, so the footer must show only the persistent state and
// never advertise sending.
func TestFooterDropsHintsDuringModal(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.setQuestion(uiQuestionRequest())
	m.layout()
	plain := strings.ToLower(sanitizeANSI(m.renderFooter()))
	if strings.Contains(plain, "enter send") {
		t.Fatalf("modal footer advertises sending: %q", plain)
	}
	if !strings.Contains(plain, "12.3k/200k") {
		t.Fatalf("modal footer lost the persistent state: %q", plain)
	}
}
