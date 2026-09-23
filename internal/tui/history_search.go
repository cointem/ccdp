package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// historySearch is the transient state of an incremental reverse search over the
// input history (ctrl+r), mirroring the readline behaviour of the sibling CLIs.
// matches holds history indices newest-first so the most recent hit is the
// default selection; sel is the cursor into matches.
type historySearch struct {
	query   string
	matches []int
	sel     int
}

// startHistorySearch opens the reverse-search prompt. It is a no-op while a
// modal owns the keys or when there is no history to search.
func (m *Model) startHistorySearch() bool {
	if m.histSearch != nil || len(m.history) == 0 {
		return false
	}
	if m.question != nil || m.approval != nil || m.picker != nil {
		return false
	}
	m.histSearch = &historySearch{}
	m.recomputeHistorySearch()
	return true
}

// recomputeHistorySearch rebuilds the match list for the current query, scanning
// newest-first, and clamps the selection. An empty query matches every entry so
// the prompt previews the most recent input before the user types.
func (m *Model) recomputeHistorySearch() {
	s := m.histSearch
	if s == nil {
		return
	}
	s.matches = s.matches[:0]
	needle := strings.ToLower(s.query)
	for i := len(m.history) - 1; i >= 0; i-- {
		if needle == "" || strings.Contains(strings.ToLower(m.history[i]), needle) {
			s.matches = append(s.matches, i)
		}
	}
	if s.sel >= len(s.matches) {
		s.sel = 0
	}
}

func (m *Model) historySearchSelected() string {
	s := m.histSearch
	if s == nil || s.sel < 0 || s.sel >= len(s.matches) {
		return ""
	}
	return m.history[s.matches[s.sel]]
}

// moveHistorySearch advances the selection by delta, wrapping within the match
// list. Positive delta walks toward older entries (readline's ctrl+r repeat).
func (m *Model) moveHistorySearch(delta int) {
	s := m.histSearch
	if s == nil || len(s.matches) == 0 {
		return
	}
	s.sel = (s.sel + delta + len(s.matches)) % len(s.matches)
}

// acceptHistorySearch loads the selected entry into the composer for editing and
// closes the prompt. It deliberately does not submit: reverse search is a recall
// aid, and the sibling CLIs let the user revise the recalled text first.
func (m *Model) acceptHistorySearch() {
	text := m.historySearchSelected()
	m.histSearch = nil
	if text == "" {
		return
	}
	m.pasteFold = nil
	m.textarea.SetValue(text)
	m.recallHistoryImages()
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.refreshCmdSuggest()
}

func (m *Model) cancelHistorySearch() {
	m.histSearch = nil
}

// handleHistorySearchKey owns the key stream while the reverse-search prompt is
// open. It returns handled=true for every key it consumes so the composer never
// sees search input.
func (m *Model) handleHistorySearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	s := m.histSearch
	if s == nil {
		return m, nil, false
	}
	m.quitArmed = false
	switch msg.String() {
	case "esc", "ctrl+c", "ctrl+g":
		m.cancelHistorySearch()
		return m, nil, true
	case "enter":
		m.acceptHistorySearch()
		return m, nil, true
	case "ctrl+r", "up":
		m.moveHistorySearch(1)
		return m, nil, true
	case "ctrl+s", "down":
		m.moveHistorySearch(-1)
		return m, nil, true
	case "backspace":
		if len(s.query) > 0 {
			s.query = s.query[:len(s.query)-1]
			m.recomputeHistorySearch()
		}
		return m, nil, true
	case "ctrl+u":
		s.query = ""
		m.recomputeHistorySearch()
		return m, nil, true
	}
	if len(msg.Runes) > 0 && msg.Type == tea.KeyRunes {
		s.query += string(msg.Runes)
		m.recomputeHistorySearch()
		return m, nil, true
	}
	// Consume every other key so it cannot reach the composer while searching.
	return m, nil, true
}

// renderHistorySearch draws the reverse-search prompt as a single row above the
// composer: the query being typed plus a preview of the selected match.
func (m *Model) renderHistorySearch() string {
	s := m.histSearch
	if s == nil {
		return ""
	}
	width := max(1, m.width-2)
	prompt := styleModalTitle.Render("(reverse-i-search)`") + styleAssistant.Render(s.query) + styleModalTitle.Render("': ")
	match := m.historySearchSelected()
	if match == "" {
		prompt += styleHints.Render("(no match)")
	} else {
		preview := strings.ReplaceAll(sanitizeANSI(match), "\n", " ")
		room := width - lipgloss.Width(prompt)
		prompt += styleStatus.Render(truncateDisplay(preview, max(8, room)))
	}
	return " " + fitLines(prompt, width)
}
