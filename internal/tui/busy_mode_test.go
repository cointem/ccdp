package tui

import (
	"testing"

	"ccdp/internal/protocol"
)

// runSlash executes the tea.Cmd returned by runCommand so the underlying
// client.Submit closure runs synchronously against the recording client.
func runSlash(t *testing.T, m *Model, text string) {
	t.Helper()
	_, cmd := m.runCommand(text)
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		if _, isErr := msg.(commandErrorMsg); isErr {
			t.Fatalf("%s produced an error msg: %+v", text, msg)
		}
	}
}

// TestBusyModeCommandSubmitsWithoutRevision is the regression probe for
// "switching /mode while the agent is busy does nothing". It drives the real
// runCommand path with m.busy=true and a nonzero (stale-prone) snapshot revision,
// and asserts the permission command actually reaches the client with NO
// optimistic-revision stamp, which previously produced a stale_revision rejection
// during an active turn.
func TestBusyModeCommandSubmitsWithoutRevision(t *testing.T) {
	m := NewWithClient(nil, "/workspace", false)
	t.Cleanup(m.watchCancel)
	m.hasSnapshot = true
	m.snapshot = protocol.SessionView{
		SessionID: protocol.SessionID(m.sessionID),
		Revision:  protocol.Revision{LogSeq: 60},
		Settings: protocol.SettingsSnapshot{
			Model:      protocol.ModelBinding{Model: "deepseek-chat"},
			Permission: protocol.PermissionPolicy{Mode: "default"},
		},
	}
	rec := &recordingClient{snapshot: m.snapshot}
	m.client = rec
	m.busy = true

	runSlash(t, &m, "/mode acceptEdits")

	var found *protocol.Command
	for i := range rec.submits {
		if rec.submits[i].Type == protocol.CommandSetPermissionPolicy {
			found = &rec.submits[i]
		}
	}
	if found == nil {
		t.Fatalf("no CommandSetPermissionPolicy submitted while busy; submits=%+v", rec.submits)
	}
	if found.ExpectedRevision != 0 {
		t.Fatalf("busy settings command carries ExpectedRevision=%d; want 0", found.ExpectedRevision)
	}
}

// TestBusyModeSelectorActionSubmits drives the selector confirm path (the flow a
// mouse/keyboard user hits after opening /mode with no args) while busy.
func TestBusyModeSelectorActionSubmits(t *testing.T) {
	m := NewWithClient(nil, "/workspace", false)
	t.Cleanup(m.watchCancel)
	m.hasSnapshot = true
	m.snapshot = protocol.SessionView{
		SessionID: protocol.SessionID(m.sessionID),
		Revision:  protocol.Revision{LogSeq: 60},
		Settings:  protocol.SettingsSnapshot{Permission: protocol.PermissionPolicy{Mode: "default"}},
	}
	rec := &recordingClient{snapshot: m.snapshot}
	m.client = rec
	m.busy = true

	if cmd := m.executeSelectorAction(selectorAction{Kind: selectorMode}, "bypassPermissions"); cmd != nil {
		_ = cmd()
	}
	var found *protocol.Command
	for i := range rec.submits {
		if rec.submits[i].Type == protocol.CommandSetPermissionPolicy {
			found = &rec.submits[i]
		}
	}
	if found == nil || found.ExpectedRevision != 0 {
		t.Fatalf("selector mode while busy: found=%+v submits=%+v", found, rec.submits)
	}
}
