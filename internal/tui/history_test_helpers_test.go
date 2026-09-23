package tui

import tea "github.com/charmbracelet/bubbletea"

// Unit tests inspect the pure planner; terminal_host and PTY tests exercise
// production delivery and acknowledgment separately.
func planTestHistory(m *Model) tea.Cmd { return m.planHistory() }
