package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestLinkScannerSpansLabelColumns(t *testing.T) {
	line := styleAssistant.Render("see ") + renderTerminalLink("docs", "https://example.com/x")
	spans := (&linkScanner{}).spans(line)
	if len(spans) != 1 {
		t.Fatalf("spans=%d, want 1 (%#v)", len(spans), spans)
	}
	got := spans[0]
	if got.target != "https://example.com/x" {
		t.Fatalf("target=%q", got.target)
	}
	// "see " is 4 cells and the label "docs" is 4 more.
	if got.start != 4 || got.end != 8 {
		t.Fatalf("span=(%d,%d), want (4,8)", got.start, got.end)
	}
}

func TestLinkScannerCarriesWrappedLabel(t *testing.T) {
	scanner := &linkScanner{}
	first := scanner.spans(osc8Prefix + "https://example.com/x" + osc8Terminator + "first")
	if len(first) != 1 || first[0].start != 0 || first[0].end != 5 {
		t.Fatalf("first row spans=%#v", first)
	}
	// The label continues on the next row; the destination stays pending so the
	// continuation cells remain clickable.
	second := scanner.spans("second" + closeOSC8)
	if len(second) != 1 || second[0].start != 0 || second[0].end != 6 {
		t.Fatalf("second row spans=%#v", second)
	}
	if second[0].target != "https://example.com/x" {
		t.Fatalf("continuation target=%q", second[0].target)
	}
	// Once closed, later rows are ordinary text again.
	if rest := scanner.spans("plain text"); len(rest) != 0 {
		t.Fatalf("spans leaked past the closing sequence: %#v", rest)
	}
}

func TestLinkScannerParsesParamsAndBelTerminator(t *testing.T) {
	line := "\x1b]8;id=42;https://example.com/y\a" + "label" + closeOSC8
	spans := (&linkScanner{}).spans(line)
	if len(spans) != 1 || spans[0].target != "https://example.com/y" || spans[0].end != 5 {
		t.Fatalf("spans=%#v", spans)
	}
}

func TestLinkScannerWideLabelColumns(t *testing.T) {
	line := renderTerminalLink("文档", "https://example.com/z")
	spans := (&linkScanner{}).spans(line)
	if len(spans) != 1 || spans[0].start != 0 || spans[0].end != 4 {
		t.Fatalf("wide label spans=%#v", spans)
	}
}

// TestClickOnLinkOpensDestination pins the reason the TUI resolves link clicks
// itself: mouse reporting is enabled, so the terminal never turns a click on a
// hyperlink into navigation.
func TestClickOnLinkOpensDestination(t *testing.T) {
	opened := stubLinkOpener(t)
	m := inlineTestModel()
	m.viewport.Style = lipgloss.NewStyle().Padding(0, 1)
	m.width, m.height = 80, 24
	m.items = []historyCell{{kind: "assistant", messageID: "a1", text: "see [docs](https://example.com/x) now"}}
	m.layout()

	if len(m.linkTargets) != 1 {
		t.Fatalf("linkTargets=%#v, want exactly one", m.linkTargets)
	}
	link := m.linkTargets[0]
	headerH := presentationHeight(m.headerPresentation(), m.width)
	leftPad := m.viewport.Style.GetHorizontalFrameSize() / 2
	y := headerH + (link.row - m.viewport.YOffset)
	x := leftPad + link.start

	cmd := clickAt(m, x, y)
	if cmd == nil {
		t.Fatal("click on the link label returned no command")
	}
	model, _ := m.Update(cmd())
	updated := modelValue(t, model)
	if len(*opened) != 1 || (*opened)[0] != "https://example.com/x" {
		t.Fatalf("opened=%v, want the link destination", *opened)
	}
	if status := noticeText(&updated); status != "" {
		t.Fatalf("successful open raised a notice: %q", status)
	}

	// The last label cell is inside the span; the cell after it is not.
	if end := clickAt(m, leftPad+link.end, y); end != nil {
		t.Fatal("click past the label end still resolved a command")
	}
	if before := clickAt(m, leftPad+link.start-1, y); before != nil {
		t.Fatal("click before the label start still resolved a command")
	}
	if len(*opened) != 1 {
		t.Fatalf("off-label clicks opened %v", *opened)
	}
}

// TestLinkLaunchFailureSurfacesNotice keeps a click from failing silently when
// no platform opener exists.
func TestLinkLaunchFailureSurfacesNotice(t *testing.T) {
	opened := stubLinkOpener(t)
	*opened = nil
	linkOpener = func(string) error { return errors.New("no opener") }

	m := inlineTestModel()
	model, _ := m.Update(linkOpenMsg{target: "https://example.com/x", err: errors.New("no opener")})
	updated := modelValue(t, model)
	if status := noticeText(&updated); !strings.Contains(status, "https://example.com/x") {
		t.Fatalf("failure notice=%q, want the destination", status)
	}
}

// stubLinkOpener replaces the process launcher so a test click records its
// destination instead of starting a real application.
func stubLinkOpener(t *testing.T) *[]string {
	t.Helper()
	previous := linkOpener
	opened := &[]string{}
	linkOpener = func(target string) error {
		*opened = append(*opened, target)
		return nil
	}
	t.Cleanup(func() { linkOpener = previous })
	return opened
}
