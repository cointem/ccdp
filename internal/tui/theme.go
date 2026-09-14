package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Keep the terminal's own background, with readable accents on light and dark
// themes. The Gopher and composer share Go blue; amber means work/attention.
var (
	colorGo      = lipgloss.AdaptiveColor{Light: "#007D9C", Dark: "#5CD5EB"}
	colorText    = lipgloss.AdaptiveColor{Light: "#29343D", Dark: "#E0E6EB"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#667580", Dark: "#8D9DA8"}
	colorBorder  = lipgloss.AdaptiveColor{Light: "#B7CBD1", Dark: "#39515B"}
	colorAmber   = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#E9B875"}
	colorSuccess = lipgloss.AdaptiveColor{Light: "#247447", Dark: "#8CD5A3"}
	colorError   = lipgloss.AdaptiveColor{Light: "#BE3246", Dark: "#F28B94"}
	colorSelect  = lipgloss.AdaptiveColor{Light: "#E1F3F7", Dark: "#173C47"}

	styleBrand      = lipgloss.NewStyle().Foreground(colorGo).Bold(true)
	styleHeader     = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 1)
	styleUser       = lipgloss.NewStyle().Foreground(colorGo).Bold(true)
	styleAssistant  = lipgloss.NewStyle().Foreground(colorText)
	styleSystem     = lipgloss.NewStyle().Foreground(colorText)
	styleToolRun    = lipgloss.NewStyle().Foreground(colorAmber)
	styleToolOK     = lipgloss.NewStyle().Foreground(colorSuccess)
	styleToolErr    = lipgloss.NewStyle().Foreground(colorError)
	styleToolOutput = lipgloss.NewStyle().Foreground(colorText)
	styleStatus     = lipgloss.NewStyle().Foreground(colorMuted)
	styleError      = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	styleHints      = lipgloss.NewStyle().Foreground(colorMuted)
	styleDivider    = lipgloss.NewStyle().Foreground(colorBorder)
	styleModal      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAmber).Padding(1, 2).Width(56)
	styleModalTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorAmber)
	styleRunning     = lipgloss.NewStyle().Foreground(colorAmber).Bold(true)
	styleCmdSug      = lipgloss.NewStyle().Foreground(colorText)
	styleCmdSugSel   = lipgloss.NewStyle().Bold(true).Foreground(colorGo).Background(colorSelect)
	styleContextOK   = styleStatus
	styleContextWarn = styleRunning
	// Markdown uses the same semantic palette as the rest of the transcript;
	// code and links get a restrained accent so their meaning remains visible
	// when colors are disabled.
	styleMarkdownCode = lipgloss.NewStyle().Foreground(colorGo)
	styleMarkdownLink = lipgloss.NewStyle().Foreground(colorGo).Underline(true)
)

func welcomeItem(workspace string) logItem {
	if workspace == "" {
		workspace = "."
	}
	return logItem{kind: "welcome", text: workspace}
}

func renderWelcome(workspace string, width int) string {
	return renderWelcomeSize(workspace, width, 0)
}

// The welcome is a one-time, compact introduction. The original mascot remains
// available as an asset, but ordinary startup reserves its space for messages.
func renderWelcomeSize(workspace string, width, height int) string {
	width = max(1, width)
	if height <= 0 {
		height = 5
	}
	workspace = strings.Join(strings.Fields(sanitizeANSI(workspace)), " ")
	if workspace == "" {
		workspace = "."
	}
	rows := []string{
		styleBrand.Render("ccdp") + styleStatus.Render("  v"+appVersion),
		styleStatus.Render(truncateDisplay(workspace, width)),
		"",
		styleAssistant.Render("What would you like to build?"),
		styleBrand.Render("/help") + styleHints.Render(" commands · ") + styleBrand.Render("/model") + styleHints.Render(" models"),
	}
	return fitLines(strings.Join(rows[:min(len(rows), height)], "\n"), width)
}

func (m *Model) renderLogItem(item *logItem, width int) string {
	if item.kind == "welcome" {
		return renderWelcomeSize(item.text, width, m.viewport.Height)
	}
	return renderItemWidth(item, width)
}

// Truncate styled chrome by terminal cells, never bytes or ANSI fragments.
// Transcript text is wrapped separately so no conversation content is lost.
func fitLines(text string, width int) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, width), "…")
	}
	return strings.Join(lines, "\n")
}
