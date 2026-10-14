package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

// openWatchCmd establishes a new terminal subscription. Watch itself emits
// an atomic initial snapshot, so resync recovery never has a snapshot/watch
// race. Connection recovery retries observation only, never submissions.
func (m Model) openWatchCmd() tea.Cmd {
	client := m.client
	if client == nil {
		return nil
	}
	ctx := m.watchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	sid := protocol.SessionID(m.sessionID)
	generation := m.watchGeneration
	cursor := m.watchCursor
	return func() tea.Msg {
		sub, err := client.Watch(ctx, cursor)
		return watchOpenedMsg{sub: sub, sessionID: sid, generation: generation, err: err}
	}
}

func readWatchCmd(sub protocol.Subscription, sid protocol.SessionID, generation uint64) tea.Cmd {
	if sub == nil {
		return nil
	}
	return func() tea.Msg {
		update, ok := <-sub.Updates()
		if !ok {
			return watchClosedMsg{sub: sub, sessionID: sid, generation: generation}
		}
		return watchUpdateMsg{sub: sub, update: update, sessionID: sid, generation: generation}
	}
}

func (m *Model) handleWatchOpened(msg watchOpenedMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		if msg.sub != nil {
			_ = msg.sub.Close()
		}
		return m, nil
	}
	if msg.err != nil {
		m.pushStatus("watch unavailable: " + msg.err.Error())
		return m, m.scheduleReconnect()
	}
	m.subscription = msg.sub
	return m, readWatchCmd(msg.sub, msg.sessionID, msg.generation)
}

func (m *Model) handleWatchClosed(msg watchClosedMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		return m, nil
	}
	if m.subscription != msg.sub {
		return m, nil
	}
	m.subscription = nil
	return m, m.scheduleReconnect()
}

func (m *Model) handleWatchUpdate(msg watchUpdateMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		return m, nil
	}
	if msg.sub != m.subscription {
		return m, nil
	}
	u := msg.update
	m.watchCursor = u.Cursor
	if u.Revision.LogSeq > m.watchCursor.LogSeq {
		m.watchCursor.LogSeq = u.Revision.LogSeq
	}
	if u.Type == protocol.UpdateResyncRequired {
		_ = msg.sub.Close()
		m.subscription = nil
		m.watchGeneration++
		return m, m.openWatchCmd()
	}
	if u.Child != nil {
		// The root stream multiplexes each child's current session row. Fold it
		// into the directory so the parent's Task marker tracks the child live;
		// the child's own transcript never enters the parent conversation.
		m.applyChildUpdate(u.Child.Session)
	}
	if u.State != nil && u.Type == protocol.UpdateState {
		view := m.snapshot
		view.ApplyState(*u.State)
		m.applySnapshot(view)
	}
	if u.Catalog != nil {
		m.snapshot.Catalog = *u.Catalog
	}
	if u.Snapshot != nil && u.Type == protocol.UpdateSnapshot {
		m.watchDisconnected = false
		m.watchRetry = 0
		m.applySnapshot(*u.Snapshot)
	}
	if u.Receipt != nil && u.Type == protocol.UpdateReceipt {
		m.applyReceipt(*u.Receipt, "")
	}
	if u.Event != nil {
		m.handleProtocolEvent(*u.Event)
	}
	resetNativeHistory := m.nativeHistoryResetPending
	m.nativeHistoryResetPending = false
	m.renderVisibleTranscript()
	cmds := []tea.Cmd{readWatchCmd(msg.sub, msg.sessionID, msg.generation), m.flushInline()}
	if resetNativeHistory {
		// The erase sequence is written before the renderer repaints, so the
		// discarded conversation cannot reappear in a later frame.
		cmds = append(cmds, m.eraseNativeHistory())
	}
	return m, tea.Batch(cmds...)
}
