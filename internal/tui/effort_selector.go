package tui

import (
	"math"
	"strings"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// "default" is a reset to ccdp's built-in effort (medium), not a level.
var effortOptions = []SelectorOption{
	{ID: "default", Label: "default", Description: "恢复内置默认 medium"},
	{ID: "none", Label: "none", Description: "关闭显式推理"},
	{ID: "minimal", Label: "minimal", Description: "最少推理"},
	{ID: "low", Label: "low", Description: "较低推理强度"},
	{ID: "medium", Label: "medium", Description: "中等推理强度"},
	{ID: "high", Label: "high", Description: "较高推理强度"},
	{ID: "xhigh", Label: "xhigh", Description: "更高推理强度"},
	{ID: "max", Label: "max", Description: "最大推理档位"},
	{ID: "ultra", Label: "ultra", Description: "超高推理档位"},
}

func (m *Model) startGenerationSelector(name string) tea.Cmd {
	options := append([]SelectorOption(nil), effortOptions...)
	current, kind, title := m.snapshot.Settings.ReasoningEffort, selectorEffort, "Reasoning effort"
	if name == "verbosity" {
		current, kind, title = m.snapshot.Settings.Verbosity, selectorVerbosity, "Response verbosity"
		options = []SelectorOption{{ID: "default", Label: "default", Description: "使用模型默认配置"}, {ID: "low", Label: "low", Description: "简短"}, {ID: "medium", Label: "medium", Description: "适中"}, {ID: "high", Label: "high", Description: "详细"}}
	}
	if current == "" {
		current = "default"
	}
	selected := 0
	for i := range options {
		if options[i].ID == current {
			selected = i
			options[i].Current = true
		}
	}
	cmd := m.startSelectorAt(title, options, selected, true, selectorAction{Kind: kind})
	if kind == selectorEffort {
		m.picker.effortMotion = effortMotion{from: float64(selected), to: float64(selected), frame: effortMotionFrames}
	}
	return cmd
}

const effortMotionFrames = 5

// The token prevents a late frame from animating a reopened/replaced selector.
type effortMotion struct {
	token    protocol.CommandID
	from, to float64
	frame    int
}
type effortTickMsg struct{ token protocol.CommandID }

func effortTick(token protocol.CommandID) tea.Cmd {
	return tea.Tick(30*time.Millisecond, func(time.Time) tea.Msg { return effortTickMsg{token: token} })
}
func (a effortMotion) position() float64 {
	t := float64(a.frame) / effortMotionFrames
	t = 1 - (1-t)*(1-t)
	return a.from + (a.to-a.from)*t
}
func (m *Model) adjustEffort(msg tea.KeyMsg) tea.Cmd {
	p := m.picker
	next := p.Index
	switch msg.String() {
	case "left":
		next = max(0, next-1)
	case "right":
		next = min(len(p.Options)-1, next+1)
	case "home":
		next = 0
	case "end":
		next = len(p.Options) - 1
	default:
		return nil
	}
	if next == p.Index {
		return nil
	}
	from := p.effortMotion.position()
	p.Index = next
	p.effortMotion = effortMotion{token: nextUICommandID(), from: from, to: float64(next)}
	return effortTick(p.effortMotion.token)
}
func (m *Model) advanceEffortMotion(msg effortTickMsg) tea.Cmd {
	if m.picker == nil || m.picker.action.Kind != selectorEffort || m.picker.effortMotion.token != msg.token {
		return nil
	}
	motion := &m.picker.effortMotion
	if motion.frame >= effortMotionFrames {
		return nil
	}
	motion.frame++
	if motion.frame < effortMotionFrames {
		return effortTick(msg.token)
	}
	return nil
}

func (m *Model) renderEffortSelector() string {
	p := m.picker
	width := max(1, m.width-2)
	// Equal-width stops keep the pointer directly above its label. Narrow
	// terminals show a moving window instead of wrapping the scale.
	slot := 9
	count := min(len(p.Options), max(1, min(90, width)/slot))
	first := max(0, min(p.Index-count/2, len(p.Options)-count))
	last := first + count
	scaleWidth := count * slot
	centers := func(i int) int { return (i-first)*slot + slot/2 }
	pointer := int(math.Round((p.effortMotion.position()-float64(first))*float64(slot))) + slot/2
	pointer = max(0, min(scaleWidth-1, pointer))
	axis := styleDivider.Render(strings.Repeat("─", pointer)) + styleBrand.Render("▲") + styleDivider.Render(strings.Repeat("─", scaleWidth-pointer-1))
	var labels strings.Builder
	for i := first; i < last; i++ {
		label := p.Options[i].Label
		styled := styleHints
		if i == p.Index {
			styled = styleBrand
		}
		left := centers(i) - (i-first)*slot - lipgloss.Width(label)/2
		labels.WriteString(strings.Repeat(" ", max(0, left)) + styled.Render(label) + strings.Repeat(" ", max(0, slot-left-lipgloss.Width(label))))
	}
	leftEnd, rightEnd := "Faster", "Smarter"
	if first > 0 {
		leftEnd = "‹ " + leftEnd
	}
	if last < len(p.Options) {
		rightEnd += " ›"
	}
	ends := leftEnd + strings.Repeat(" ", max(1, scaleWidth-lipgloss.Width(leftEnd)-lipgloss.Width(rightEnd))) + rightEnd
	if count == 1 {
		ends = "Effort"
	}
	center := func(text string) string { return lipgloss.NewStyle().Width(width).Align(lipgloss.Center).Render(text) }
	selected := p.Options[p.Index]
	note := selected.Description
	if selected.ID == m.snapshot.Settings.ReasoningEffort || selected.ID == "default" && m.snapshot.Settings.ReasoningEffort == "" {
		note += " · current"
	}
	warning := "更高强度可能增加 token 用量和响应时间"
	hint := "←/→ adjust · Enter confirm · Esc cancel"
	if width < 40 {
		hint = "←/→ · Enter 确认 · Esc 取消"
	}
	rows := []string{styleBrand.Render(strings.Repeat("─", width)), styleBrand.Render("Effort"), "",
		center(styleHints.Render(ends)), center(axis), center(labels.String()),
		center(styleStatus.Render(note)), center(styleHints.Render(warning)), "", styleHints.Render(hint)}
	if m.height > 0 && m.height < 18 {
		rows = []string{styleBrand.Render("Effort"), center(styleHints.Render(ends)), center(axis), center(labels.String()), styleHints.Render(hint)}
	}
	return strings.Join(rows, "\n")
}
