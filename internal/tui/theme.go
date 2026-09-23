package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ThemeTokens gives all components the same semantic color contract.
type ThemeTokens struct {
	Accent, Text, Muted, Border, Warning, Success, Error lipgloss.AdaptiveColor
	Input, Selection, Added, Removed                     lipgloss.AdaptiveColor
}

var theme = ThemeTokens{
	Accent:    lipgloss.AdaptiveColor{Light: "#0969A2", Dark: "#80BFFF"},
	Text:      lipgloss.AdaptiveColor{Light: "#24292F", Dark: "#D4D7E5"},
	Muted:     lipgloss.AdaptiveColor{Light: "#656D76", Dark: "#9298AE"},
	Border:    lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#484D61"},
	Warning:   lipgloss.AdaptiveColor{Light: "#8A5900", Dark: "#D9BD82"},
	Success:   lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#A3CB78"},
	Error:     lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F08090"},
	Input:     lipgloss.AdaptiveColor{Light: "#EAECF0", Dark: "#30323D"},
	Selection: lipgloss.AdaptiveColor{Light: "#DDEBFF", Dark: "#303D54"},
	Added:     lipgloss.AdaptiveColor{Light: "#DAFBE1", Dark: "#203B2B"},
	Removed:   lipgloss.AdaptiveColor{Light: "#FFEBE9", Dark: "#48251F"},
}
var (
	colorGo              = theme.Accent
	colorText            = theme.Text
	colorMuted           = theme.Muted
	colorBorder          = theme.Border
	colorAmber           = theme.Warning
	colorSuccess         = theme.Success
	colorError           = theme.Error
	colorInputBackground = theme.Input

	styleBrand       = lipgloss.NewStyle().Foreground(colorGo).Bold(true)
	styleHeader      = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 1)
	styleUser        = lipgloss.NewStyle().Foreground(colorText)
	styleAssistant   = lipgloss.NewStyle().Foreground(colorText)
	styleSystem      = lipgloss.NewStyle().Foreground(colorText)
	styleToolRun     = lipgloss.NewStyle().Foreground(colorAmber)
	styleAgentRun    = lipgloss.NewStyle().Foreground(colorGo)
	styleToolOK      = lipgloss.NewStyle().Foreground(colorSuccess)
	styleToolErr     = lipgloss.NewStyle().Foreground(colorError)
	styleToolOutput  = lipgloss.NewStyle().Foreground(colorText)
	styleStatus      = lipgloss.NewStyle().Foreground(colorMuted)
	styleError       = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	styleHints       = lipgloss.NewStyle().Foreground(colorMuted)
	styleDivider     = lipgloss.NewStyle().Foreground(colorBorder)
	styleModalTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorAmber)
	styleRunning     = lipgloss.NewStyle().Foreground(colorAmber).Bold(true)
	styleCmdSug      = lipgloss.NewStyle().Foreground(colorText)
	styleCmdSugSel   = lipgloss.NewStyle().Bold(true).Foreground(colorText).Background(theme.Selection)
	styleContextOK   = styleStatus
	styleContextWarn = styleRunning
	// Markdown uses the same semantic palette as the rest of the transcript;
	// code and links get a restrained accent so their meaning remains visible
	// when colors are disabled.
	styleMarkdownCode = lipgloss.NewStyle().Foreground(colorGo)
	styleMarkdownLink = lipgloss.NewStyle().Foreground(colorGo).Underline(true)

	// Intra-line word diff styles
	styleDiffAddedWord   = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Underline(true)
	styleDiffRemovedWord = lipgloss.NewStyle().Foreground(colorError).Bold(true).Underline(true)
)

func welcomeItem(workspace string) historyCell {
	if workspace == "" {
		workspace = "."
	}
	return historyCell{kind: "welcome", text: workspace, reportID: "welcome"}
}

func renderWelcome(workspace string, width int) string {
	return renderWelcomeSize(workspace, width, 0)
}

// The welcome is a one-time, compact introduction.
func renderWelcomeSize(workspace string, width, height int) string {
	return renderWelcomeContext(workspace, "", width, height)
}

func renderWelcomeContext(workspace, model string, width, height int) string {
	width = max(1, width)
	if height <= 0 {
		height = 6
	}
	workspace = sanitizeANSI(shortenPath(workspace))
	if workspace == "" {
		workspace = "."
	}
	rows := []string{styleAssistant.Bold(true).Render("ccdp") + styleHints.Render("  v"+appVersion)}
	if model != "" {
		rows = append(rows, styleHints.Render("model:     ")+styleAssistant.Render(sanitizeANSI(model)))
	}
	rows = append(rows, styleHints.Render("directory: ")+styleAssistant.Render(workspace),
		styleHints.Render("/help commands · /model models"))
	if height >= len(rows)+2 && width >= 32 {
		boxWidth := min(64, width)
		for i := range rows {
			rows[i] = ansi.Truncate(rows[i], max(1, boxWidth-4), "…")
		}
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(theme.Border).
			Padding(0, 1).Width(boxWidth - 2).Render(strings.Join(rows, "\n"))
	}
	return fitLines(strings.Join(rows[:min(height, len(rows))], "\n"), width)
}

func (m *Model) renderLogItem(item *historyCell, width int) string {
	if item.kind == "assistant" {
		return renderAssistantCell(item.cleanText(), width, m.workspace)
	}
	if item.kind == "welcome" {
		return renderWelcomeContext(item.text, m.modelName, width, m.viewport.Height)
	}
	if item.kind == "tool" {
		return m.renderToolView(item, width)
	}
	if item.kind == "thinking" {
		return m.renderThinkingView(item, width)
	}
	return renderItemWidth(item, width)
}

func (m *Model) renderThinkingView(item *historyCell, width int) string {
	view := renderThinkingCell(item, width)
	if (thinkingInProgress(item)) && m.animateWork() {
		view = strings.Replace(view, styleStatus.Render("• Thinking…"), m.spinner.View()+" "+styleStatus.Render("Thinking…"), 1)
	}
	return view
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

func isDarkTheme() bool {
	return lipgloss.HasDarkBackground()
}

// Keep raw reasoning in the expanded cell/reader, not in the collapsed header.
func renderThinkingHeader(item *historyCell) string {
	if thinkingInProgress(item) {
		return styleStatus.Render("• Thinking…")
	}
	return styleStatus.Render("• Thought")
}

// renderThinkingDetails retains the expanded transcript presentation. Placement
// belongs to the overlay; it must not rewrite or reformat the detail contents.
func renderThinkingDetails(item *historyCell, width int) string {
	rows := []string{renderThinkingHeader(item) + "  " + styleHints.Render("[click to collapse]")}
	lines := strings.Split(strings.TrimRight(item.cleanText(), "\n"), "\n")
	limit := min(25, len(lines))
	for _, line := range lines[:limit] {
		rows = append(rows, "  "+styleStatus.Render(truncateDisplay(line, max(1, width-4))))
	}
	if len(lines) > limit {
		rows = append(rows, styleHints.Render(fmt.Sprintf("  … (%d more lines)", len(lines)-limit)))
	}
	return strings.Join(rows, "\n")
}

// Live reasoning is visible as it arrives. Only settled reasoning collapses;
// its complete source remains available to the detail overlay and transcript.
func renderThinkingCell(item *historyCell, width int) string {
	header := renderThinkingHeader(item)
	if !thinkingInProgress(item) {
		return header
	}
	text := item.cleanText()
	if text == "" {
		return header
	}
	rows := []string{header}
	for _, line := range wrapDisplay(text, max(1, width-2)) {
		rows = append(rows, "  "+styleStatus.Render(line))
	}
	return strings.Join(rows, "\n")
}

func thinkingInProgress(item *historyCell) bool {
	return item.status == "running" || item.status == "streaming"
}
