package tui

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

// TestConversationResetErasesNativeHistory pins the /clear contract. The
// runtime replaces the conversation projection and republishes a same-session
// snapshot with no transcript; every row of the discarded conversation —
// including UI-local reports such as /status output — must leave the screen,
// and the native scrollback that holds them must be erased, which no managed
// frame repaint can reach.
func TestConversationResetErasesNativeHistory(t *testing.T) {
	m := NewWithClient(nil, "", false)
	t.Cleanup(m.watchCancel)
	view := protocolSnapshot("session", protocol.MessageView{ID: "m1", Role: "assistant", Content: "hello"})
	m.applySnapshot(view)
	m.pushLog("system", "/status\npubfyu7vgogno2octtqnkes2rr  2026-09-23 22:59")
	if len(m.confirmedItems) == 0 || len(m.reports) == 0 {
		t.Fatalf("precondition: transcript=%d reports=%d", len(m.confirmedItems), len(m.reports))
	}
	var output bytes.Buffer
	m.terminal = NewTerminalHost(&output)

	cleared := protocolSnapshot("session")
	cleared.Revision.LogSeq = view.Revision.LogSeq
	m.applySnapshot(cleared)

	if !m.nativeHistoryResetPending {
		t.Fatal("a conversation reset did not arm native-history erasure")
	}
	if len(m.reports) != 0 || len(m.items) != 0 || len(m.confirmedItems) != 0 {
		t.Fatalf("discarded conversation survived the reset: reports=%d items=%d confirmed=%d",
			len(m.reports), len(m.items), len(m.confirmedItems))
	}
	if m.inline.seen(historyCell{messageID: "m1"}, 0) {
		t.Fatal("reset kept the discarded row's print identity")
	}
	if command := m.eraseNativeHistory(); command == nil || !strings.Contains(output.String(), "\x1b[3J") {
		t.Fatalf("reset did not erase native scrollback: cmd=%v output=%q", command, output.String())
	}
	// The re-baselined ledger must not re-print the discarded conversation, and
	// the conversation that follows must still reach native scrollback.
	if command := m.planHistory(); command != nil {
		t.Fatal("reset re-printed the discarded conversation")
	}
	m.applyTranscript([]protocol.TranscriptItem{{ID: "m2", Kind: "assistant", Text: "fresh", Status: "completed"}})
	if command := m.planHistory(); command == nil {
		t.Fatal("post-reset transcript was not printable")
	}
}

// TestWatchConversationResetErasesNativeHistory drives the same reset through
// the production update path: the runtime's state update is what erases the
// discarded scrollback, and the pending flag must not survive into later frames.
func TestWatchConversationResetErasesNativeHistory(t *testing.T) {
	client := &recordingClient{snapshot: protocolSnapshot("session", protocol.MessageView{ID: "m1", Role: "assistant", Content: "hello"})}
	m := NewWithClient(client, "", false)
	t.Cleanup(m.watchCancel)
	m.applySnapshot(client.snapshot)
	m.pushLog("system", "/status\npubfyu7vgogno2octtqnkes2rr  2026-09-23 22:59")
	sub, err := client.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	m.subscription = sub
	var output bytes.Buffer
	m.terminal = NewTerminalHost(&output)

	cleared := protocolSnapshot("session")
	cleared.Revision.LogSeq = m.snapshot.Revision.LogSeq
	next, _ := m.handleWatchUpdate(watchUpdateMsg{sub: sub, update: protocol.Update{
		Type: protocol.UpdateState, Snapshot: &cleared, Revision: cleared.Revision},
		sessionID: protocol.SessionID(m.sessionID), generation: m.watchGeneration})
	m = modelValue(t, next)

	if m.nativeHistoryResetPending {
		t.Fatal("native-history reset stayed armed after the state update")
	}
	if !strings.Contains(output.String(), "\x1b[3J") {
		t.Fatalf("state update did not erase native scrollback: %q", output.String())
	}
	if len(m.reports) != 0 || len(m.confirmedItems) != 0 {
		t.Fatalf("discarded conversation survived the state update: reports=%d confirmed=%d", len(m.reports), len(m.confirmedItems))
	}
}

// recordingClient makes protocol-bound UI tests deterministic without
// starting an Agent or touching a user's session directory.
type recordingClient struct {
	mu       sync.Mutex
	snapshot protocol.SessionView
	submits  []protocol.Command
	receipt  *protocol.Receipt
	watchers []*recordingSubscription
}

func (c *recordingClient) publish(update protocol.Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, watcher := range c.watchers {
		watcher.mu.Lock()
		if !watcher.closed {
			watcher.ch <- update
		}
		watcher.mu.Unlock()
	}
}

func (c *recordingClient) submitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.submits)
}

func (c *recordingClient) Submit(_ context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submits = append(c.submits, cmd)
	if c.receipt != nil {
		r := *c.receipt
		r.CommandID, r.SessionID = cmd.ID, cmd.SessionID
		return r, nil
	}
	r := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptApplied, Revision: c.snapshot.Revision}
	return r, nil
}

func (c *recordingClient) Snapshot(context.Context) (protocol.SessionView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot, nil
}

func (c *recordingClient) Watch(context.Context, protocol.Cursor) (protocol.Subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &recordingSubscription{ch: make(chan protocol.Update, 8)}
	s.ch <- protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &c.snapshot, Revision: c.snapshot.Revision,
		Cursor: protocol.Cursor{LogSeq: c.snapshot.Revision.LogSeq, ViewGeneration: c.snapshot.Revision.ViewGeneration}}
	c.watchers = append(c.watchers, s)
	return s, nil
}

type recordingSubscription struct {
	mu     sync.Mutex
	ch     chan protocol.Update
	closed bool
}

func (s *recordingSubscription) Updates() <-chan protocol.Update { return s.ch }
func (s *recordingSubscription) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	return nil
}

func applyTeaCmd(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	next, follow := m.Update(msg)
	*m = next.(Model)
	if follow != nil {
		// Tests intentionally do not run watcher commands recursively. Core
		// command receipts are single-step messages.
	}
}

func protocolSnapshot(session string, history ...protocol.MessageView) protocol.SessionView {
	return protocol.SessionView{
		SessionID: protocol.SessionID(session),
		Revision:  protocol.Revision{LogSeq: uint64(len(history))},
		Settings: protocol.SettingsSnapshot{
			Model:      protocol.ModelBinding{Model: "test-model"},
			Permission: protocol.PermissionPolicy{Mode: "default"},
			Sandbox:    protocol.SandboxPolicy{},
		},
		History:    history,
		Transcript: testTranscript(history),
	}
}

func testTranscript(history []protocol.MessageView) []protocol.TranscriptItem {
	items := make([]protocol.TranscriptItem, 0, len(history))
	for i, msg := range history {
		id := msg.ID
		if id == "" {
			id = "fixture-" + strconv.Itoa(i)
		}
		items = append(items, protocol.TranscriptItem{ID: id, Kind: msg.Role, Text: msg.Content, Status: "completed"})
	}
	return items
}

func modelValue(t *testing.T, model tea.Model) Model {
	t.Helper()
	switch value := model.(type) {
	case Model:
		return value
	case *Model:
		if value == nil {
			t.Fatal("Update returned a nil *Model")
		}
		return *value
	default:
		t.Fatalf("Update returned %T, want Model or *Model", model)
		return Model{}
	}
}

func TestModelUpdateExecutesTypedPickerActionOnReturnedModel(t *testing.T) {
	client := &recordingClient{snapshot: protocolSnapshot("test-session")}
	m := NewWithClient(client, t.TempDir(), false)
	m.width, m.height = 80, 24
	m.baseVpH = 19
	m.snapshot.Catalog.Providers = []protocol.ProviderSnapshot{{ID: "test", Models: []string{"alpha", "beta"}}}
	// The fixture's confirmed binding is the first catalog entry, so one Down
	// has a stable, meaningful target (beta) even when the picker also retains
	// a binding that is absent from a provider catalog.
	m.snapshot.Settings.Model.Model = "alpha"
	m.modelName = "alpha"
	m.textarea.SetValue("/model")
	_, _ = m.submit()
	if m.picker == nil {
		t.Fatal("/model should open a picker")
	}

	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	moved := modelValue(t, model)
	if moved.picker == nil || moved.picker.Index != 1 {
		t.Fatalf("Update did not retain picker selection: %#v", moved.picker)
	}
	model, cmd = moved.Update(tea.KeyMsg{Type: tea.KeyEnter})
	selected := modelValue(t, model)
	if selected.picker != nil {
		t.Fatal("enter should close the picker on the returned model")
	}
	if cmd == nil {
		t.Fatal("typed picker action should return a Submit command")
	}
	result := cmd()
	model, _ = selected.Update(result)
	selected = modelValue(t, model)
	if len(client.submits) != 1 || client.submits[0].Type != protocol.CommandSetModel || client.submits[0].Model.Model != "beta" {
		t.Fatalf("picker action did not submit beta: %#v", client.submits)
	}
	if noticeText(&selected) != "model → beta" {
		t.Fatalf("receipt feedback = %q, want model → beta", noticeText(&selected))
	}
}

func TestPickerStableIDsScrollAndTinyTerminal(t *testing.T) {
	client := &recordingClient{snapshot: protocolSnapshot("test-session")}
	m := NewWithClient(client, t.TempDir(), false)
	m.width, m.height = 24, 8
	m.baseVpH = 4
	options := make([]SelectorOption, 45)
	for i := range options {
		options[i] = SelectorOption{ID: "stable-" + strconv.Itoa(i), Label: "item " + strconv.Itoa(i)}
	}
	m.startSelectorAt("Many options", options, 0, false, selectorAction{Kind: selectorModel})
	if m.picker == nil || m.picker.Options[0].ID != "stable-0" || m.picker.Options[44].ID != "stable-44" {
		t.Fatalf("selector did not retain stable option ids: %#v", m.picker)
	}
	for range 12 {
		model, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = modelValue(t, model)
	}
	if m.picker.Index != 12 {
		t.Fatalf("arrow navigation stopped at first page: %d", m.picker.Index)
	}
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	m = modelValue(t, model)
	if m.picker.Index <= 12 {
		t.Fatalf("fn/alt down did not advance the selector: %d", m.picker.Index)
	}
	if got := lipgloss.Height(m.renderInlineSurface()); got > m.height {
		t.Fatalf("tiny selector height %d exceeds terminal height %d", got, m.height)
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = modelValue(t, model)
	if m.picker != nil {
		t.Fatal("escape should dismiss the selector")
	}
}

func TestReportsRemainAnchoredAcrossSnapshotResync(t *testing.T) {
	initial := protocolSnapshot("test-session", protocol.MessageView{ID: "assistant-1", Role: "assistant", Content: "old"})
	m := NewWithClient(&recordingClient{snapshot: initial}, t.TempDir(), false)
	m.snapshot = protocol.SessionView{}
	m.hasSnapshot = false
	m.applySnapshot(initial)
	m.addReport(historyCell{kind: "system", text: "config report"})
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, SessionID: "test-session", MessageID: "user-1", Text: "new prompt"})
	m.addReport(historyCell{kind: "error", text: "turn failed"})
	m.appendTranscript(historyCell{kind: "assistant", text: "partial"})
	resynced := initial
	resynced.Revision.LogSeq = 4
	resynced.History = []protocol.MessageView{
		{ID: "assistant-1", Role: "assistant", Content: "old"},
		{ID: "user-1", Role: "user", Content: "new prompt"},
		{ID: "assistant-2", Role: "assistant", Content: "partial"},
	}
	resynced.Transcript = testTranscript(resynced.History)
	m.applySnapshot(resynced)
	if len(m.items) != 5 {
		t.Fatalf("merged transcript has %d items: %#v", len(m.items), m.items)
	}
	want := []string{"old", "config report", "new prompt", "turn failed", "partial"}
	for i, text := range want {
		if m.items[i].text != text {
			t.Fatalf("item %d = %q, want %q; items=%#v", i, m.items[i].text, text, m.items)
		}
	}
	resynced.Transcript = testTranscript(resynced.History)
	m.applySnapshot(resynced)
	if len(m.items) != 5 {
		t.Fatalf("identical resync duplicated or dropped items: %#v", m.items)
	}
}

func TestUserEventWithDurableIDDoesNotDuplicateSnapshotMessage(t *testing.T) {
	m := *sugModel()
	snapshot := protocolSnapshot("test-session", protocol.MessageView{ID: "user-1", Role: "user", Content: "same text"})
	m.snapshot = protocol.SessionView{}
	m.hasSnapshot = false
	m.applySnapshot(snapshot)
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, SessionID: "test-session",
		MessageID: "user-1", Text: "same text"})
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].messageID != "user-1" {
		t.Fatalf("same durable user event duplicated: %#v", m.confirmedItems)
	}
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, SessionID: "test-session",
		MessageID: "user-2", Text: "same text"})
	if len(m.confirmedItems) != 2 {
		t.Fatalf("different durable ids should remain distinct: %#v", m.confirmedItems)
	}
}

func TestApprovalReceiptOnlyClosesMatchingModal(t *testing.T) {
	m := sugModel()
	m.approval = &approvalPrompt{ID: "danger-1", Tool: "Bash", Command: "rm -i file"}
	m.approvalPending = true
	m.pendingApprovalCommand = "approval-1"
	m.applyReceipt(protocol.Receipt{CommandID: "other", SessionID: "test-session", Status: protocol.ReceiptApplied}, "other command")
	if m.approval == nil || !m.approvalPending {
		t.Fatal("unrelated success closed the approval modal")
	}
	m.applyReceipt(protocol.Receipt{CommandID: "approval-1", SessionID: "test-session", Status: protocol.ReceiptRejected}, "")
	if m.approval == nil || m.approvalPending {
		t.Fatal("matching rejection should stop pending state while retaining the modal")
	}
	m.approvalPending = true
	m.pendingApprovalCommand = "approval-2"
	m.pendingApprovalKey = m.approvalKey()
	m.applyReceipt(protocol.Receipt{CommandID: "approval-2", SessionID: "test-session", Status: protocol.ReceiptApplied}, "")
	if m.approval != nil || m.approvalPending {
		t.Fatal("matching success should close the approval modal")
	}
}

func TestRejectedInputRestoresDraftAndReusesCommandID(t *testing.T) {
	m := *sugModel()
	client := m.client.(*recordingClient)
	client.receipt = &protocol.Receipt{Status: protocol.ReceiptRejected,
		Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "busy"}}
	m.textarea.SetValue("retry this exact draft")
	_, cmd := m.submit()
	if cmd == nil {
		t.Fatal("input submission did not produce a protocol command")
	}
	applyTeaCmd(&m, cmd)
	if len(client.submits) != 1 {
		t.Fatalf("input command was not submitted: %#v", client.submits)
	}
	firstID := client.submits[0].ID
	if got := m.textarea.Value(); got != "retry this exact draft" {
		t.Fatalf("rejected input did not restore draft: %q", got)
	}
	if m.pendingSubmissionCount() != 0 || m.retryCommandID != firstID {
		t.Fatalf("retry record missing or still in-flight: pending=%#v retry=%q", m.operations, m.retryCommandID)
	}
	_, retryCmd := m.submit()
	if retryCmd == nil {
		t.Fatal("restored draft did not produce a retry command")
	}
	_ = retryCmd()
	if len(client.submits) != 2 || client.submits[1].ID != firstID {
		t.Fatalf("logical retry changed command id: first=%q retry=%q", firstID, client.submits[1].ID)
	}
}

func TestRejectedInputClearsPendingRecordWithNewDraft(t *testing.T) {
	m := sugModel()
	client := m.client.(*recordingClient)
	client.receipt = &protocol.Receipt{Status: protocol.ReceiptRejected,
		Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "busy"}}
	m.textarea.SetValue("old draft")
	_, cmd := m.submit()
	if cmd == nil {
		t.Fatal("input submission did not produce a command")
	}
	_ = cmd()
	firstID := client.submits[0].ID
	m.textarea.SetValue("new draft")
	m.applyReceipt(protocol.Receipt{CommandID: firstID, SessionID: "test-session", Status: protocol.ReceiptRejected,
		Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "busy"}}, "")
	if got := m.textarea.Value(); got != "new draft" {
		t.Fatalf("new draft was overwritten: %q", got)
	}
	if m.pendingSubmissionCount() != 0 {
		t.Fatalf("rejected old draft remained in-flight while new draft was active: %#v", m.operations)
	}
	if m.retryCommandID != firstID || m.retryDraft != "old draft" {
		t.Fatalf("failed old draft lost from retry slot: command=%q draft=%q", m.retryCommandID, m.retryDraft)
	}
}

func TestSubmissionTransportFailureClearsPendingAndRestoresRetry(t *testing.T) {
	m := sugModel()
	commandID := protocol.CommandID("transport-failure")
	m.registerOperation(protocol.NewSubmitInput(commandID, protocol.SessionID(m.sessionID), protocol.InputID(commandID), "retry this message", protocol.InputSteer), "message submitted")
	m.textarea.Reset()

	model, _ := m.Update(commandErrorMsg{
		commandID: commandID,
		sessionID: protocol.SessionID(m.sessionID),
		err:       context.Canceled,
	})
	got := modelValue(t, model)
	if got.pendingSubmissionCount() != 0 {
		t.Fatalf("transport failure left in-flight submission: %#v", got.operations)
	}
	if got.textarea.Value() != "retry this message" {
		t.Fatalf("transport failure did not restore draft: %q", got.textarea.Value())
	}
	if got.retryCommandID != commandID || got.retryDraft != "retry this message" {
		t.Fatalf("retry slot = (%q,%q), want failed command and draft", got.retryCommandID, got.retryDraft)
	}
}

func TestDuplicateReceiptProducesOnlyOneReport(t *testing.T) {
	m := sugModel()
	receipt := protocol.Receipt{CommandID: "cmd-duplicate", SessionID: "test-session", Status: protocol.ReceiptRejected,
		Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "busy"}}
	m.applyReceipt(receipt, "")
	m.applyReceipt(receipt, "")
	if len(m.reports) != 1 || len(m.items) != 1 {
		t.Fatalf("duplicate receipt changed view twice: reports=%#v items=%#v", m.reports, m.items)
	}
}

func TestReceiptDisplayDedupCacheIsBounded(t *testing.T) {
	m := sugModel()
	for i := 0; i < maxReceiptKeys+128; i++ {
		m.applyReceipt(protocol.Receipt{
			CommandID: protocol.CommandID("receipt-" + strconv.Itoa(i)),
			SessionID: "test-session",
			Status:    protocol.ReceiptApplied,
			Revision:  protocol.Revision{LogSeq: uint64(i + 1)},
		}, "")
	}
	if got := len(m.receiptKeys); got > maxReceiptKeys {
		t.Fatalf("UI receipt cache grew to %d, limit %d", got, maxReceiptKeys)
	}
	if got := len(m.receiptOrder); got > maxReceiptKeys {
		t.Fatalf("UI receipt order grew to %d, limit %d", got, maxReceiptKeys)
	}
	if _, ok := m.receiptKeys["receipt-0"]; ok {
		t.Fatal("old display receipt identity was not evicted")
	}
}

func TestApprovalDecisionCannotBeReenteredWhilePending(t *testing.T) {
	m := sugModel()
	m.approval = &approvalPrompt{ID: "approval-1", Tool: "Bash", Command: "echo ok", Reason: "test"}
	m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyDown})
	model, first := m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnter})
	*m = modelValue(t, model)
	if first == nil || !m.approvalPending {
		t.Fatal("first approval decision was not submitted")
	}
	if _, second := m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnter}); second != nil {
		t.Fatal("approval modal allowed a second decision while first was pending")
	}
	if got := len(m.client.(*recordingClient).submits); got != 0 {
		t.Fatalf("approval key synchronously submitted %d commands", got)
	}
}

func TestSubmissionShowsPendingStatusUntilReceipt(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("hello")
	model, cmd := m.submit()
	*m = modelValue(t, model)
	if cmd == nil || noticeText(m) != "submitting…" {
		t.Fatalf("submission status=%q command=%v, want pending status and command", noticeText(m), cmd != nil)
	}
	result := cmd()
	model, _ = m.Update(result)
	*m = modelValue(t, model)
	if noticeText(m) != "message submitted" {
		t.Fatalf("receipt status=%q, want terminal submission status", noticeText(m))
	}
}

// newLifecycleTestAgent builds a real runtime handle entirely below TempDir.
// HOME is redirected as well because Agent construction loads project/user
// skills; the resume cleanup test must never inspect the developer's account.
func newLifecycleTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.NoSessionPersistence = true
	cfg.APIKey = "test-key"
	cfg.BaseURL = "http://127.0.0.1:1"
	ag, err := agent.New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func assertAgentClosed(t *testing.T, ag *agent.Agent) {
	t.Helper()
	select {
	case <-ag.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("resume candidate was not closed")
	}
}

func TestResumeQueuedCandidateIsClosedDuringModelClose(t *testing.T) {
	candidate := newLifecycleTestAgent(t)
	task := &resumeTaskState{done: make(chan struct{})}
	task.finish(candidate)
	if !task.cancelIfNotStarted() {
		t.Fatal("queued resume task was not cancellable")
	}
	task.waitAndCloseUnclaimed()
	assertAgentClosed(t, candidate)
}

func TestResumeStartedCandidateIsJoinedAndClosedDuringModelClose(t *testing.T) {
	candidate := newLifecycleTestAgent(t)
	task := &resumeTaskState{done: make(chan struct{})}
	if !task.begin() {
		t.Fatal("resume task did not start")
	}
	joined := make(chan struct{})
	go func() {
		task.waitAndCloseUnclaimed()
		close(joined)
	}()
	select {
	case <-joined:
		t.Fatal("started resume task was not joined before completion")
	case <-time.After(10 * time.Millisecond):
	}
	task.finish(candidate)
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join started resume task")
	}
	assertAgentClosed(t, candidate)
}

func TestResumeOpenedConsumesPendingMouseRelease(t *testing.T) {
	source := newLifecycleTestAgent(t)
	defer source.Close()

	m := NewWithResume(source, false)
	m.sessionID = source.SessionID()
	m.hasSnapshot = true
	m.snapshot = protocolSnapshot(source.SessionID())

	// The resume picker arms mouse tracking (so the list can be wheel-scrolled)
	// and, on Enter, leaves a pending mouse release behind so the later
	// receipt/error path can disable it. Resume completes via resumeOpenedMsg
	// instead of a command receipt, so that path must consume the flag itself;
	// otherwise mouse tracking leaks and the terminal can no longer scroll its
	// native scrollback with a trackpad/mouse.
	m.mouseReleasePending = true
	model, _ := m.update(resumeOpenedMsg{
		candidate: source,
		snapshot:  protocolSnapshot(source.SessionID()),
		source:    source,
	})
	got := modelValue(t, model)
	if got.mouseReleasePending {
		t.Fatal("resumeOpenedMsg left the selector's pending mouse release armed; mouse tracking leaks after /resume")
	}
}

func TestResumeSameSessionReprimesInlineTranscript(t *testing.T) {
	source := newLifecycleTestAgent(t)
	defer source.Close()

	m := NewWithResume(source, false)
	old := historyCell{kind: "assistant", messageID: "restored-message", text: "old attachment"}
	m.items = []historyCell{old}
	m.snapshot = protocolSnapshot(source.SessionID(), protocol.MessageView{
		ID: old.messageID, Role: "assistant", Content: old.text,
	})
	m.hasSnapshot = true
	m.sessionID = source.SessionID()
	m.inline.ensure()
	m.inline.printed[itemIdentity(old, 0)] = struct{}{}
	m.inline.primed = true
	m.inline.showBaseline = false

	// The persisted session keeps its ID across a resume, but the UI
	// attachment has changed. Use the same durable ID to prove that the old
	// attachment's printed set cannot swallow the restored message.
	restored := protocolSnapshot(source.SessionID(), protocol.MessageView{
		ID: old.messageID, Role: "assistant", Content: "restored from disk",
	})
	model, _ := m.handleResumeOpened(resumeOpenedMsg{
		candidate:          source,
		snapshot:           restored,
		source:             source,
		expectedSessionID:  m.sessionID,
		expectedGeneration: m.watchGeneration,
	})
	got := modelValue(t, model)
	if !got.inline.primed || !got.inline.showBaseline {
		t.Fatalf("same-session resume did not re-prime baseline: primed=%v show=%v", got.inline.primed, got.inline.showBaseline)
	}
	if _, printed := got.inline.printed[itemIdentity(old, 0)]; printed {
		t.Fatal("same-session resume retained the old attachment's printed identity")
	}
	if cmd := planTestHistory(&got); cmd == nil {
		t.Fatal("restored same-session history was not queued to native scrollback")
	}
}
