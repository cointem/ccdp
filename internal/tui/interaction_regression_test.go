package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

func TestDisabledPickerOptionStaysOpenWithoutSubmitting(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 24
	options := []selectorOption{
		{ID: "blocked", Label: "blocked", Description: "requires a provider", Disabled: true},
		{ID: "available", Label: "available"},
	}
	m.startSelectorAt("Select model", options, 0, true, selectorAction{Kind: selectorModel})
	client := m.client.(*recordingClient)

	_, cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("disabled picker option returned a command")
	}
	if m.picker == nil {
		t.Fatal("disabled picker option closed the selector")
	}
	if got := client.submitCount(); got != 0 {
		t.Fatalf("disabled picker option submitted %d commands", got)
	}
	if m.status != "requires a provider" {
		t.Fatalf("disabled option reason = %q", m.status)
	}
}

func TestModelPickerMarksConfirmedAndPendingBindings(t *testing.T) {
	m := sugModel()
	m.snapshot.Pending = &protocol.PendingSettings{Model: &protocol.ModelBinding{Model: "pending-model"}}
	m.snapshot.Catalog.Providers[0].Models = []string{"confirmed-model", "pending-model"}
	m.modelName = "confirmed-model"
	m.snapshot.Settings.Model.Model = "confirmed-model"
	m.hasSnapshot = true

	model, _ := m.runCommand("/model")
	*m = modelValue(t, model)
	if m.picker == nil || len(m.picker.options) != 2 {
		t.Fatalf("model picker options = %#v", m.picker)
	}
	var confirmed, pending selectorOption
	for _, option := range m.picker.options {
		switch option.ID {
		case "confirmed-model":
			confirmed = option
		case "pending-model":
			pending = option
		}
	}
	if !confirmed.Current || confirmed.Pending {
		t.Fatalf("confirmed model metadata = %#v", confirmed)
	}
	if pending.Current || !pending.Pending {
		t.Fatalf("pending model metadata = %#v", pending)
	}
}

func TestFreshUserOperationRetiresOlderTerminalErrorNotice(t *testing.T) {
	m := sugModel()
	failed := protocol.NewSubmitInput("failed", protocol.SessionID(m.sessionID), "input-failed", "first", protocol.InputSteer)
	m.registerOperation(failed, "first message")
	m.updateOperation(failed.ID, OperationFailed, protocol.Receipt{CommandID: failed.ID,
		SessionID: failed.SessionID, Status: protocol.ReceiptRejected})
	m.addNotice(Notice{CommandID: failed.ID, Severity: NoticeError, Text: "first failed", Sticky: true})

	active := protocol.NewSubmitInput("active", protocol.SessionID(m.sessionID), "input-active", "second", protocol.InputSteer)
	m.submitCommand(active, "second message")
	if _, ok := m.noticeForCommand(failed.ID); ok {
		t.Fatal("older terminal error notice remained after a new user operation")
	}
	if op, ok := m.operation(active.ID); !ok || !op.Active() {
		t.Fatalf("new user operation is not active: %#v, present=%v", op, ok)
	}
	if status := m.renderStatus(); status != "" && containsDisplay(status, "first failed") {
		t.Fatalf("older error still masks active operation: %q", status)
	}
}

func TestNoticeExpiryResumesAfterIdleQueue(t *testing.T) {
	m := sugModel()
	base := time.Unix(100, 0)
	previousNow := now
	now = func() time.Time { return base }
	defer func() { now = previousNow }()

	m.addNotice(Notice{Severity: NoticeInfo, Text: "first", CreatedAt: base,
		ExpiresAt: base.Add(time.Second)})
	// The normal notice ticker stops once this queue is empty.
	model, _ := m.Update(noticeTickMsg{at: base.Add(2 * time.Second)})
	*m = modelValue(t, model)
	if len(m.notices) != 0 {
		t.Fatalf("expired notice remained after idle tick: %#v", m.notices)
	}

	m.addNotice(Notice{Severity: NoticeInfo, Text: "second", CreatedAt: base.Add(2 * time.Second),
		ExpiresAt: base.Add(3 * time.Second)})
	now = func() time.Time { return base.Add(4 * time.Second) }
	model, _ = m.Update(spinner.TickMsg{})
	*m = modelValue(t, model)
	if len(m.notices) != 0 || m.status != "" {
		t.Fatalf("notice added after idle did not expire on spinner tick: notices=%#v status=%q", m.notices, m.status)
	}
}

func containsDisplay(text, needle string) bool {
	return len(needle) > 0 && len(text) >= len(needle) &&
		stringContains(text, needle)
}

func stringContains(text, needle string) bool {
	for i := 0; i+len(needle) <= len(text); i++ {
		if text[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
