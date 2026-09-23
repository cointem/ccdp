package tui

import (
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type reconnectMsg struct {
	sessionID  string
	generation uint64
}

// Retries only reattach observation. Submitted commands are never replayed by
// connection recovery; their IDs and receipts remain owned by the protocol.
func (m *Model) scheduleReconnect() tea.Cmd {
	m.watchDisconnected = true
	if m.snapshot.Phase == protocol.PhaseClosed || m.snapshot.Closing || m.watchRetry >= 5 {
		m.pushStatus("connection closed · /reconnect · draft preserved")
		return nil
	}
	m.watchRetry++
	delay := time.Second * time.Duration(1<<uint(m.watchRetry-1))
	sid, generation := m.sessionID, m.watchGeneration
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return reconnectMsg{sessionID: sid, generation: generation}
	})
}

func (m *Model) reconnect() tea.Cmd {
	if m.subscription != nil {
		_ = m.subscription.Close()
		m.subscription = nil
	}
	m.watchGeneration++
	m.watchRetry = 0
	m.watchDisconnected = true
	return m.openWatchCmd()
}
