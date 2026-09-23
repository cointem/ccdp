package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strconv"
	"strings"
)

func (m *Model) detailAvailableRows() int {
	// Leave room for the stable composer/footer and the card's title/hint.
	base := *m
	base.detail = nil
	chrome := base.composeChrome()
	return max(1, m.height-presentationHeight(chrome.input, m.width)-presentationHeight(chrome.footer, m.width)-m.detailRow)
}

func (m *Model) detailRows() int { return m.detailAvailableRows() }

func (m *Model) moveDetail(delta int) {
	if m.detail == nil {
		return
	}
	for i := range m.items {
		item := &m.items[i]
		id := detailIdentity(item, i)
		if id == m.detail.itemID {
			m.detail.text = m.detailContent(item)
			break
		}
	}
	m.detail.lineViewport = m.detailRows()
	m.detail.lines = presentationRows(m.detail.text, max(1, m.width))
	m.detail.offset = min(m.detail.maxOffset(), max(0, m.detail.offset+delta))
}

func (m *Model) renderDetailPopover() string {
	if m.detail == nil {
		return ""
	}
	m.moveDetail(0)
	r := m.detail
	end := min(len(r.lines), r.offset+r.lineViewport)
	return strings.Join(r.lines[r.offset:end], "\n")
}

func (m *Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.detail = nil
	case "up", "k":
		m.moveDetail(-1)
	case "down", "j":
		m.moveDetail(1)
	case "pgup", "ctrl+u":
		m.moveDetail(-m.detailRows())
	case "pgdown", "ctrl+d":
		m.moveDetail(m.detailRows())
	case "home":
		m.detail.offset = 0
	case "end":
		m.moveDetail(len(m.detail.text) + 1)
	case "ctrl+o":
		m.detail = nil
		return m, m.openTranscriptReader()
	}
	return m, nil
}

func (m *Model) detailContent(item *historyCell) string {
	text := renderThinkingDetails(item, m.width)
	if item.kind == "tool" {
		p := toolPresentationFromItem(item)
		for _, key := range []string{"file_path", "path", "directory"} {
			if path, ok := p.Args[key].(string); ok {
				p.Args[key] = displayToolPath(path, m.workspace)
			}
		}
		p.Expanded = true
		text = renderToolPresentation(p, m.width)
	}

	return text
}

// Reasoning has a transcript ID, not a tool call ID. Use that stable identity
// for both click targets and live refresh so different turns cannot alias.
func detailIdentity(item *historyCell, index int) string {
	if item.kind == "thinking" {
		if item.messageID != "" {
			return "thinking:" + item.messageID
		}
		if item.toolID == "" {
			return "thinking:local:" + strconv.Itoa(index)
		}
	}
	return item.toolID
}
