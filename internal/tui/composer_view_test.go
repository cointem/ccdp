package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestComposerCodexLayout(t *testing.T) {
	for _, width := range []int{24, 40, 80, 120} {
		m := astraModel(t, width, 24)
		m.textarea.Reset()
		m.layout()
		empty := strings.Split(sanitizeANSI(m.renderInput()), "\n")
		if len(empty) != 3 || strings.TrimSpace(empty[0]) != "" || strings.TrimSpace(empty[2]) != "" {
			t.Fatalf("width %d: want one input row between two blank rows: %q", width, empty)
		}
		if !strings.HasPrefix(empty[1], "› Ask CCDP") {
			t.Fatalf("unexpected prompt: %q", empty[1])
		}
		m.textarea.SetValue("first line\n中文第二行\nthird line")
		m.syncInputHeight()
		view := sanitizeANSI(m.renderInput())
		if strings.Count(view, "›") != 1 {
			t.Fatalf("repeated prompt: %q", view)
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) != width {
				t.Fatalf("width %d: row has width %d: %q", width, lipgloss.Width(line), line)
			}
		}
		if !strings.Contains(view, "  中文第二行") {
			t.Fatalf("continuation lost gutter: %q", view)
		}
	}
}

func TestComposerBackgroundInheritedByText(t *testing.T) {
	ta := newTextarea()
	for name, style := range map[string]lipgloss.Style{
		"text": ta.FocusedStyle.Text, "placeholder": ta.FocusedStyle.Placeholder,
		"prompt": ta.FocusedStyle.Prompt, "cursor line": ta.FocusedStyle.CursorLine,
	} {
		if style.Inherit(ta.FocusedStyle.Base).GetBackground() != colorInputBackground {
			t.Fatalf("%s does not use composer background", name)
		}
	}
}
