package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"ccdp/internal/protocol"
)

// Styled chrome must remain one row even with long provider labels, Unicode
// paths, and multi-line status events. Wrapping here hides the input controls.
func TestThemeChromeStaysWithinTerminal(t *testing.T) {
	original := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(original) })
	for _, width := range []int{20, 40, 64, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := NewWithClient(nil, strings.Repeat("/项目/代码", 8), false)
			t.Cleanup(m.watchCancel)
			m.width, m.height = width, 24
			m.modelName = strings.Repeat("provider/模型", 8)
			m.pushStatus("Reading…\n" + strings.Repeat("进展", 100) + "\x1b[2J")
			m.busy = true
			m.turnStarted = time.Now().Add(-75 * time.Second)
			m.layout()
			for name, output := range map[string]string{
				"header": m.renderHeader(), "status": m.renderStatus(), "footer": m.renderFooter(),
			} {
				if !utf8.ValidString(output) || lipgloss.Width(output) > width || lipgloss.Height(output) != 1 {
					t.Errorf("%s exceeds %d-column row: %q", name, width, output)
				}
				if strings.Contains(output, "\x1b[2J") {
					t.Errorf("%s kept an injected terminal command", name)
				}
			}
		})
	}
}

func TestWelcomeReflowsAndCommitsOnce(t *testing.T) {
	for _, width := range []int{16, 24, 48, 64, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := NewWithClient(&recordingClient{snapshot: protocol.SessionView{SessionID: "fresh"}}, "/代码/ccdp", false)
			t.Cleanup(m.watchCancel)
			m.width, m.height = width, 28
			m.layout()
			if len(m.items) != 1 || m.items[0].kind != "welcome" {
				t.Fatalf("fresh session missing welcome: %+v", m.items)
			}
			welcome := renderItemWidth(&m.items[0], width-2)
			if lipgloss.Width(welcome) > width-2 || !strings.Contains(welcome, "ccdp") {
				t.Fatalf("welcome does not fit: %q", welcome)
			}
			if planTestHistory(&m) == nil {
				t.Fatal("welcome not committed to scrollback")
			}
			m.width = max(16, width/2)
			m.layout()
			if planTestHistory(&m) != nil {
				t.Fatal("resize duplicated the committed welcome")
			}
		})
	}
}

func TestBusyFrameLeavesRoomForComposerAndSuggestions(t *testing.T) {
	m := NewWithClient(nil, "/ccdp", false)
	t.Cleanup(m.watchCancel)
	m.width, m.height = 64, 20
	m.busy = true
	m.items = []historyCell{{kind: "assistant", text: strings.Repeat("streamed line\n", 60)}}
	m.textarea.SetValue("/")
	m.layout()
	m.refreshCmdSuggest()
	if got := lipgloss.Height(m.View()); got > m.height {
		t.Fatalf("suggestions pushed composer out of terminal: %d > %d", got, m.height)
	}
	if !strings.Contains(m.View(), "❯") {
		t.Fatal("busy frame lost composer")
	}
}

func TestWelcomeStaysCompactAtEveryTerminalSize(t *testing.T) {
	for _, size := range [][2]int{{16, 12}, {48, 24}, {48, 32}, {80, 24}, {88, 32}, {120, 40}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := NewWithClient(nil, "/workspace/ccdp", false)
			t.Cleanup(m.watchCancel)
			m.width, m.height = size[0], size[1]
			m.items = []historyCell{welcomeItem(m.workspace)}
			m.layout()
			banner := m.renderLogItem(&m.items[0], m.width-2)
			if lipgloss.Height(banner) > m.viewport.Height || lipgloss.Width(banner) > m.width-2 {
				t.Fatalf("welcome does not fit %v: %dx%d", size, lipgloss.Width(banner), lipgloss.Height(banner))
			}
			if !strings.Contains(banner, "ccdp") || lipgloss.Height(m.View()) > m.height {
				t.Fatal("welcome lost brand or pushed composer off-screen")
			}
			if lipgloss.Height(banner) > 5 {
				t.Fatal("default welcome must leave room for conversation")
			}
			if strings.ContainsAny(sanitizeANSI(banner), "▀▄█") {
				t.Fatal("default welcome unexpectedly rendered the full mascot")
			}
		})
	}
}
