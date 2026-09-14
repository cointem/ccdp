package tui

import (
	"math"
	"strings"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Provider-default is a reset, not a level of reasoning intensity.
var effortOptions = []selectorOption{
	{ID: "default", Label: "default", Description: "由模型默认配置决定"},
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
	options := append([]selectorOption(nil), effortOptions...)
	current, kind, title := m.snapshot.Settings.ReasoningEffort, selectorEffort, "Reasoning effort"
	if name == "verbosity" {
		current, kind, title = m.snapshot.Settings.Verbosity, selectorVerbosity, "Response verbosity"
		options = []selectorOption{{ID: "default", Label: "default", Description: "使用模型默认配置"}, {ID: "low", Label: "low", Description: "简短"}, {ID: "medium", Label: "medium", Description: "适中"}, {ID: "high", Label: "high", Description: "详细"}}
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
	next := p.index
	switch msg.String() {
	case "left":
		next = max(0, next-1)
	case "right":
		next = min(len(p.options)-1, next+1)
	case "home":
		next = 0
	case "end":
		next = len(p.options) - 1
	default:
		return nil
	}
	if next == p.index {
		return nil
	}
	from := p.effortMotion.position()
	p.index = next
	p.selector.Index = next
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
	current := m.snapshot.Settings.ReasoningEffort
	if current == "" {
		current = "default"
	}
	rows := []string{styleModalTitle.Render(truncateDisplay("Effort · current "+current, width))}
	// Show a sliding neighbourhood on narrow terminals, keeping navigation
	// horizontal instead of wrapping the options into a vertical menu.
	first, last := p.index, p.index+1
	measure := func(a, b int) int {
		n := 0
		for i := a; i < b; i++ {
			n += len(p.options[i].Label) + 3
		}
		return n + 4
	}
	for first > 0 || last < len(p.options) {
		moved := false
		if first > 0 && measure(first-1, last) <= width {
			first--
			moved = true
		}
		if last < len(p.options) && measure(first, last+1) <= width {
			last++
			moved = true
		}
		if !moved {
			break
		}
	}
	labels := []string{}
	for i := first; i < last; i++ {
		label := p.options[i].Label
		if i == p.index {
			label = styleCmdSugSel.Render("[" + label + "]")
		} else {
			label = styleHints.Render(" " + label + " ")
		}
		labels = append(labels, label)
	}
	left, right := " ", " "
	if first > 0 {
		left = "‹"
	}
	if last < len(p.options) {
		right = "›"
	}
	rows = append(rows, truncateDisplay(left+strings.Join(labels, " ")+right, width))
	if p.index == 0 {
		rows = append(rows, styleHints.Render("model default"))
	} else {
		cells := min(24, max(8, width-3))
		ratio := max(0, p.effortMotion.position()-1) / float64(len(p.options)-2)
		filled := min(cells, int(math.Round(ratio*float64(cells))))
		rows = append(rows, styleRunning.Render(strings.Repeat("━", filled))+styleHints.Render(strings.Repeat("─", cells-filled)))
	}
	selected := p.options[p.index]
	note := selected.Description
	if selected.ID == current {
		note += " · ✓"
	}
	rows = append(rows, styleStatus.Render(truncateDisplay(note, width)))
	hint := "←/→ adjust · Enter confirm · Esc cancel"
	if width < 40 {
		hint = "←/→ · Enter 确认 · Esc 取消"
	}
	rows = append(rows, styleHints.Render(lipgloss.NewStyle().Width(width).Render(hint)))
	return strings.Join(rows, "\n")
}
