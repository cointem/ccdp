package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func testPersistenceConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.Model = "test-model"
	return cfg
}

func TestAgentPersistenceProjectionRoundTrip(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "round-trip", time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(101, 0).UTC()
	history := []messages.Message{
		{Role: messages.RoleSystem, Content: "system", CreatedAt: time.Unix(1, 0).UTC()},
		{Role: messages.RoleUser, Content: "hello", CreatedAt: created},
		{Role: messages.RoleAssistant, Content: "world", CreatedAt: time.Unix(102, 0).UTC()},
		{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "call-1", Name: "Read", Arguments: map[string]any{"file_path": "x"}}}, CreatedAt: time.Unix(103, 0).UTC()},
		{Role: messages.RoleTool, ToolCallID: "call-1", Content: "ok", CreatedAt: time.Unix(104, 0).UTC()},
	}
	if err := p.persistSettingsSnapshot(session.Settings{Workspace: cfg.Workspace, Model: cfg.Model}, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.persistUsage(Usage{InputTokens: 12, OutputTokens: 7, CachedTokens: 2, Cost: 0.75, TurnCount: 3}); err != nil {
		t.Fatal(err)
	}
	if err := p.persistInputQueued("queued-1", "later", "followup", "", created); err != nil {
		t.Fatal(err)
	}
	if err := p.persistHistoryMessages(history, "turn-1"); err != nil {
		t.Fatal(err)
	}
	snapshot := SessionSnapshot{ID: "round-trip", CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(105, 0).UTC(), Workspace: cfg.Workspace, Model: cfg.Model, History: history, Usage: Usage{InputTokens: 12, OutputTokens: 7, CachedTokens: 2, Cost: 0.75, TurnCount: 3}, Pending: []string{"later"}}
	if err := p.saveProjection(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(cfg.SessionDir, "round-trip.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy JSON path should not be written, stat err=%v", err)
	}
	got, err := LoadSession(cfg.SessionDir, "round-trip")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace != snapshot.Workspace || got.Model != snapshot.Model {
		t.Fatalf("settings = %q/%q, want %q/%q", got.Workspace, got.Model, snapshot.Workspace, snapshot.Model)
	}
	if len(got.History) != len(history) {
		t.Fatalf("history length = %d, want %d: %+v", len(got.History), len(history), got.History)
	}
	for i := range history {
		if got.History[i].Role != history[i].Role || got.History[i].Content != history[i].Content || !got.History[i].CreatedAt.Equal(history[i].CreatedAt) {
			t.Fatalf("history[%d] = %+v, want %+v", i, got.History[i], history[i])
		}
	}
	if got.History[3].ToolCalls[0].Name != "Read" || got.History[3].ToolCalls[0].Arguments["file_path"] != "x" {
		t.Fatalf("tool call was not preserved: %+v", got.History[3])
	}
	if len(got.Pending) != 1 || got.Pending[0] != "later" {
		t.Fatalf("pending = %v", got.Pending)
	}
	if len(got.PendingInputs) != 1 || got.PendingInputs[0].ID != "queued-1" || got.PendingInputs[0].Strategy != protocol.InputFollowup {
		t.Fatalf("typed pending = %+v", got.PendingInputs)
	}
	if got.Usage != snapshot.Usage {
		t.Fatalf("usage = %+v, want %+v", got.Usage, snapshot.Usage)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionDir, "round-trip", "snapshot.json")); err != nil {
		t.Fatalf("snapshot cache missing: %v", err)
	}
}

func TestLegacyResumeImportIsExplicitAndReadOnlyListing(t *testing.T) {
	cfg := testPersistenceConfig(t)
	snap := SessionSnapshot{ID: "legacy-one", CreatedAt: time.Unix(200, 0).UTC(), UpdatedAt: time.Unix(201, 0).UTC(), Workspace: cfg.Workspace, Model: cfg.Model,
		History: []messages.Message{{Role: messages.RoleUser, Content: "old", CreatedAt: time.Unix(202, 0).UTC()}, {Role: messages.RoleAssistant, Content: "answer", CreatedAt: time.Unix(203, 0).UTC()}},
		Usage:   Usage{InputTokens: 3, OutputTokens: 4, CachedTokens: 1, Cost: 0.2, TurnCount: 1}, Pending: []string{"queued"}}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(cfg.SessionDir, snap.ID+".json")
	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(legacyPath)
	listed, err := ListSessions(cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != snap.ID {
		t.Fatalf("legacy list = %+v", listed)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionDir, snap.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only listing created new session dir: %v", err)
	}
	loaded, err := LoadSession(cfg.SessionDir, snap.ID)
	if err != nil || len(loaded.History) != 2 {
		t.Fatalf("legacy load = %+v, err=%v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionDir, snap.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only load created new session dir: %v", err)
	}
	if err := prepareResumePersistence(&cfg, snap); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(legacyPath)
	if string(after) != string(before) {
		t.Fatal("legacy JSON was modified during import")
	}
	got, err := LoadSession(cfg.SessionDir, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || len(got.Pending) != 1 || got.Usage != snap.Usage {
		t.Fatalf("imported projection = %+v, want history/pending/usage from legacy", got)
	}
	if got.History[0].CreatedAt != snap.History[0].CreatedAt || got.History[1].CreatedAt != snap.History[1].CreatedAt {
		t.Fatalf("import lost message timestamps: %+v", got.History)
	}
}

func TestSessionPersistenceLockReleasedByClose(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "locked", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.OpenJSONLStore(cfg.SessionDir, "locked"); !errors.Is(err, session.ErrWriterLocked) {
		t.Fatalf("second writer err = %v, want ErrWriterLocked", err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	second, err := session.OpenJSONLStore(cfg.SessionDir, "locked")
	if err != nil {
		t.Fatalf("writer did not release after close: %v", err)
	}
	_ = second.Close()
}

func TestReplayStartedToolIsUnknownWithoutRerun(t *testing.T) {
	store := session.NewMemoryStore()
	defer store.Close()
	created := time.Unix(300, 0).UTC()
	_, err := store.Commit(session.Beginning, session.Batch{TransactionID: "tool-recovery", Events: []session.Event{
		session.SessionCreated{SessionID: "recover", FormatVersion: session.SchemaVersion, CreatedAt: created},
		session.AssistantCommitted{TurnID: "turn-1", Message: session.Message{MessageID: "assistant-1", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentToolCall, ToolCall: &session.ToolCall{CallID: "call-1", ToolID: "Bash", Arguments: []byte(`{"command":"touch x"}`)}}}}},
		session.ToolStarted{TurnID: "turn-1", Call: session.ToolCall{CallID: "call-1", ToolID: "Bash", Arguments: []byte(`{"command":"touch x"}`)}, Fingerprint: "fingerprint"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := SessionSnapshotProjection(store, "recover")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || got.History[1].Role != messages.RoleTool || got.History[1].ToolCallID != "call-1" {
		t.Fatalf("unknown tool recovery = %+v", got.History)
	}
	if got.History[1].Content == "" || !strings.Contains(got.History[1].Content, "unknown") {
		t.Fatalf("unknown result text = %q", got.History[1].Content)
	}
}

func TestLegacyImportPublishesOnlyAfterCompletion(t *testing.T) {
	cfg := testPersistenceConfig(t)
	snap := SessionSnapshot{ID: "legacy-large", CreatedAt: time.Unix(400, 0).UTC(), UpdatedAt: time.Unix(401, 0).UTC(), Workspace: cfg.Workspace, Model: cfg.Model}
	for i := 0; i < 70; i++ {
		snap.History = append(snap.History, messages.Message{Role: messages.RoleUser, Content: "same text", CreatedAt: time.Unix(int64(500+i), 0).UTC()})
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.SessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(cfg.SessionDir, snap.ID+".json")
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(sourcePath)

	// Simulate a crash after the first data chunk. The completion marker has
	// not been committed, so the directory must not become authoritative.
	events, err := legacyEvents(snap, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := openSessionPersistenceSource(&cfg, snap.ID, snap.CreatedAt, "legacy-import")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) <= 129 {
		t.Fatalf("test import should require more than one chunk: %d events", len(events))
	}
	if _, err := p.Commit(session.Batch{TransactionID: legacyImportBatch, Events: events[:128]}); err != nil {
		_ = p.close()
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSession(cfg.SessionDir, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.History) != len(snap.History) {
		t.Fatalf("incomplete import should read legacy history: got %d, want %d", len(loaded.History), len(snap.History))
	}

	if err := prepareResumePersistence(&cfg, snap); err != nil {
		t.Fatal(err)
	}
	// A retry after a completed import is a no-op, not a duplicate append.
	if err := prepareResumePersistence(&cfg, snap); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSession(cfg.SessionDir, snap.ID); err != nil {
		t.Fatal(err)
	} else if len(got.History) != len(snap.History) {
		t.Fatalf("completed import history = %d, want %d", len(got.History), len(snap.History))
	} else {
		seen := make(map[string]bool, len(got.History))
		for _, message := range got.History {
			if message.ID == "" || seen[message.ID] {
				t.Fatalf("legacy message IDs are not stable/unique: %+v", got.History)
			}
			seen[message.ID] = true
		}
	}
	after, _ := os.ReadFile(sourcePath)
	if string(after) != string(before) {
		t.Fatal("legacy source changed during resumed import")
	}
}

func TestNewRefusesExistingSessionAndResumeUsesProjection(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "existing", time.Unix(600, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	cfg.SessionID = "existing"
	if ag, err := New(&cfg, make(chan Event, 4)); err == nil {
		ag.Close()
		t.Fatal("New attached to an existing session")
	}
	snap, err := LoadSession(cfg.SessionDir, "existing")
	if err != nil {
		t.Fatal(err)
	}
	ag, err := Resume(&cfg, snap, make(chan Event, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if ag.SessionID() != "existing" {
		t.Fatalf("resumed session id = %q", ag.SessionID())
	}
}

func TestOpenSessionPreparesIndependentHandleWithoutMutatingSource(t *testing.T) {
	cfg := testPersistenceConfig(t)
	source, err := New(&cfg, make(chan Event, 8))
	if err != nil {
		t.Fatal(err)
	}
	source.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "source", CreatedAt: time.Unix(650, 0).UTC()})
	if err := source.Save(); err != nil {
		t.Fatal(err)
	}
	sourceID := source.SessionID()
	if err := source.closePersistence(); err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	opened, err := source.OpenSession(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = opened.closePersistence()
		opened.Close()
	}()
	if opened == source || opened.SessionID() != sourceID || len(opened.History()) != 1 {
		t.Fatalf("opened handle = %p/%q history=%d", opened, opened.SessionID(), len(opened.History()))
	}
	if source.SessionID() != sourceID || len(source.History()) != 1 {
		t.Fatalf("source mutated while opening: id=%q history=%d", source.SessionID(), len(source.History()))
	}
}

func TestOpenSessionLockFailureLeavesSourceUntouched(t *testing.T) {
	cfg := testPersistenceConfig(t)
	source, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.appendHistory(messages.Message{Role: messages.RoleUser, Content: "source", CreatedAt: time.Unix(675, 0).UTC()})
	if err := source.Save(); err != nil {
		t.Fatal(err)
	}
	oldID := source.SessionID()
	oldCfg := source.configSnapshot()
	oldHistory := source.History()

	target, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if targetID == oldID {
		t.Fatal("test sessions unexpectedly share an ID")
	}
	_, err = source.OpenSession(targetID)
	if !errors.Is(err, session.ErrWriterLocked) {
		t.Fatalf("OpenSession locked target error = %v, want ErrWriterLocked", err)
	}
	if source.SessionID() != oldID || !reflect.DeepEqual(source.History(), oldHistory) {
		t.Fatalf("source changed after failed open: id=%q history=%+v", source.SessionID(), source.History())
	}
	if got := source.configSnapshot(); got.Workspace != oldCfg.Workspace || got.Model != oldCfg.Model {
		t.Fatalf("source config changed after failed open: %+v", got)
	}

	if err := target.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := source.OpenSession(targetID)
	if err != nil {
		t.Fatalf("OpenSession after target close: %v", err)
	}
	if opened.events != nil {
		t.Fatal("opened session inherited source event channel")
	}
	openedCfg := opened.configSnapshot()
	openedCfg.Model = "changed-only-in-opened"
	opened.cfg.Model = openedCfg.Model
	if source.configSnapshot().Model == openedCfg.Model {
		t.Fatal("opened config aliases source config")
	}
	if err := opened.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCloseResumeCloseUsesDistinctLifecycleTransactions(t *testing.T) {
	cfg := testPersistenceConfig(t)
	first, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := first.SessionID()
	if err := first.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := LoadSession(cfg.SessionDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resume(&cfg, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	store, err := session.OpenJSONLReadOnly(cfg.SessionDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	records, readErr := session.ReadAll(store)
	closeErr := store.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	var closeTransactions []string
	for _, record := range records {
		if record.Event.Type() == session.EventTypeSessionClosed {
			closeTransactions = append(closeTransactions, record.TransactionID)
		}
	}
	if len(closeTransactions) != 2 {
		t.Fatalf("SessionClosed facts = %v, want two lifecycle facts", closeTransactions)
	}
	if closeTransactions[0] == closeTransactions[1] {
		t.Fatalf("reopened lifecycle reused transaction ID %q", closeTransactions[0])
	}
}

func TestHistoryClearDoesNotResurrectOldProjection(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "history-clear", time.Unix(700, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	history := []messages.Message{{Role: messages.RoleUser, Content: "a", CreatedAt: time.Unix(701, 0).UTC()}, {Role: messages.RoleAssistant, Content: "b", CreatedAt: time.Unix(702, 0).UTC()}}
	if err := p.persistHistoryMessages(history, "turn-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.persistHistoryMessages(nil, "rewind"); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(cfg.SessionDir, "history-clear")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 0 {
		t.Fatalf("cleared history resurrected: %+v", got.History)
	}
}

func TestHistoryAThenBThenAIsNotSwallowedByTransactionDedup(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "history-cycle", time.Unix(725, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	historyA := []messages.Message{{Role: messages.RoleUser, Content: "A", CreatedAt: time.Unix(726, 0).UTC()}}
	historyB := []messages.Message{{Role: messages.RoleUser, Content: "B", CreatedAt: time.Unix(727, 0).UTC()}}
	if err := p.persistHistoryMessages(historyA, "command-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.persistHistoryMessages(historyB, "command-b"); err != nil {
		t.Fatal(err)
	}
	if err := p.persistHistoryMessages(historyA, "command-a-again"); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(cfg.SessionDir, "history-cycle")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0].Content != "A" {
		t.Fatalf("A→B→A projection = %+v", got.History)
	}
}

func TestAgentPendingAndDeliveredInputsSurviveRestartWithoutRerun(t *testing.T) {
	cfg := testPersistenceConfig(t)
	events := make(chan Event, 32)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(800, 0).UTC()
	if err := ag.persistInput("input-pending", "same", "followup", "", created); err != nil {
		t.Fatal(err)
	}
	if err := ag.persistInput("input-delivered", "same", "followup", "", created.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The runtime's typed claim path records delivery before the turn starts;
	// this test supplies that fact directly so the remaining input is the only
	// paused item in the restart projection.
	if err := ag.persistInputDelivered("input-pending", "turn-1"); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	ag.pendingMsgs = []string{"same"}
	ag.history = []messages.Message{{Role: messages.RoleUser, Content: "same", CreatedAt: created.Add(2 * time.Second)}}
	ag.mu.Unlock()
	if err := ag.Save(); err != nil {
		t.Fatal(err)
	}
	// One queue item remains paused; the other is represented by its durable
	// InputDelivered fact. Identical text must not collapse the two IDs.
	if err := ag.closePersistence(); err != nil {
		t.Fatal(err)
	}
	ag.Close()

	snap, err := LoadSession(cfg.SessionDir, ag.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.History) != 1 || len(snap.Pending) != 1 {
		t.Fatalf("restart projection history=%d pending=%v", len(snap.History), snap.Pending)
	}
	if len(snap.PendingInputs) != 1 || snap.PendingInputs[0].ID != "input-delivered" {
		t.Fatalf("restart typed pending = %+v", snap.PendingInputs)
	}
	resumed, err := Resume(&cfg, snap, make(chan Event, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = resumed.closePersistence()
		resumed.Close()
	}()
	resumed.mu.Lock()
	if resumed.busy || len(resumed.pendingMsgs) != 1 || len(resumed.seenInputs) != 2 {
		resumed.mu.Unlock()
		t.Fatalf("resumed runtime state busy=%v pending=%v seen=%d", resumed.busy, resumed.pendingMsgs, len(resumed.seenInputs))
	}
	resumed.mu.Unlock()
	before := len(resumed.pendingMsgs)
	receipt := resumed.applySubmitInput(protocol.Command{ID: "retry-command", Input: &protocol.SubmitInput{ID: "input-delivered", Text: "same", Strategy: protocol.InputFollowup}})
	if receipt.Rejected() {
		t.Fatalf("durable duplicate was rejected: %+v", receipt)
	}
	resumed.mu.Lock()
	after := len(resumed.pendingMsgs)
	busy := resumed.busy
	resumed.mu.Unlock()
	if after != before || busy {
		t.Fatalf("durable duplicate started work: before=%d after=%d busy=%v", before, after, busy)
	}
}

func TestAgentSaveUsesMemoryStoreWhenPersistenceDisabled(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	ag, err := New(&cfg, make(chan Event, 8))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ag.closePersistence()
		ag.Close()
	}()
	ag.appendHistory(messages.Message{Role: messages.RoleUser, Content: "memory", CreatedAt: time.Unix(900, 0).UTC()})
	if err := ag.Save(); err != nil {
		t.Fatal(err)
	}
	p := ag.persistenceHandle()
	if p == nil {
		t.Fatal("memory persistence missing")
	}
	got, err := SessionSnapshotProjection(p.store, ag.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0].ID == "" {
		t.Fatalf("memory projection = %+v", got.History)
	}
}

func TestPopulateMemoryResumePreservesSnapshotIdentityAndInbox(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	p, err := openSessionPersistence(&cfg, "memory-seed", time.Unix(1000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	created := time.Unix(1001, 0).UTC()
	pendingAt := time.Unix(1002, 0).UTC()
	snapshot := &SessionSnapshot{
		ID: "memory-seed", CreatedAt: created, UpdatedAt: pendingAt,
		Workspace: cfg.Workspace, Model: cfg.Model,
		History: []messages.Message{
			{ID: "user-stable", Role: messages.RoleUser, Content: "hello", CreatedAt: created.Add(time.Second)},
			{ID: "assistant-stable", Role: messages.RoleAssistant, Content: "world", CreatedAt: created.Add(2 * time.Second)},
		},
		Usage:         Usage{InputTokens: 9, OutputTokens: 4, CachedTokens: 2, Cost: 0.5, TurnCount: 1},
		Pending:       []string{"queued"},
		PendingInputs: []protocol.InputView{{ID: "pending-stable", Text: "queued", Strategy: protocol.InputSteer, State: "queued", CreatedAt: pendingAt}},
		ParentID:      "parent-session", BranchPoint: 1, BranchSummary: "already tried",
	}
	if err := p.populateMemoryResume(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := SessionSnapshotProjection(p.Store(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || got.History[0].ID != "user-stable" || got.History[1].ID != "assistant-stable" {
		t.Fatalf("memory history identity = %+v", got.History)
	}
	if !got.History[0].CreatedAt.Equal(snapshot.History[0].CreatedAt) || !got.History[1].CreatedAt.Equal(snapshot.History[1].CreatedAt) {
		t.Fatalf("memory history timestamps = %+v", got.History)
	}
	if len(got.PendingInputs) != 1 || got.PendingInputs[0].ID != "pending-stable" || got.PendingInputs[0].Strategy != protocol.InputSteer || !got.PendingInputs[0].CreatedAt.Equal(pendingAt) {
		t.Fatalf("memory typed pending = %+v", got.PendingInputs)
	}
	if got.Usage != snapshot.Usage || got.ParentID != snapshot.ParentID || got.BranchPoint != snapshot.BranchPoint || got.BranchSummary != snapshot.BranchSummary {
		t.Fatalf("memory metadata = %+v", got)
	}
	if err := p.populateMemoryResume(snapshot); err != nil {
		t.Fatal(err)
	}
	again, err := SessionSnapshotProjection(p.Store(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("memory seed was not idempotent:\nfirst=%+v\nagain=%+v", got, again)
	}
}

func TestResumeNoPersistenceRetainsProvidedHistoryAndPending(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	created := time.Unix(1100, 0).UTC()
	snapshot := &SessionSnapshot{
		ID: "memory-resume-agent", CreatedAt: created, UpdatedAt: created.Add(time.Second),
		Workspace: cfg.Workspace, Model: cfg.Model,
		History:       []messages.Message{{ID: "resume-user", Role: messages.RoleUser, Content: "keep me", CreatedAt: created.Add(2 * time.Second)}},
		Usage:         Usage{InputTokens: 5, OutputTokens: 6, TurnCount: 1},
		Pending:       []string{"wait me"},
		PendingInputs: []protocol.InputView{{ID: "resume-pending", Text: "wait me", Strategy: protocol.InputFollowup, State: "queued", CreatedAt: created.Add(3 * time.Second)}},
	}
	ag, err := Resume(&cfg, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	history := ag.History()
	if len(history) != 1 || history[0].ID != "resume-user" || history[0].Content != "keep me" {
		t.Fatalf("resumed memory history = %+v", history)
	}
	ag.mu.Lock()
	pending := cloneInputViews(ag.pendingInputs)
	pendingTexts := append([]string(nil), ag.pendingMsgs...)
	ag.mu.Unlock()
	if len(pending) != 1 || pending[0].ID != "resume-pending" || pending[0].Strategy != protocol.InputFollowup {
		t.Fatalf("resumed memory typed pending = %+v", pending)
	}
	if len(pendingTexts) != 1 || pendingTexts[0] != "wait me" {
		t.Fatalf("resumed memory compatibility pending = %v", pendingTexts)
	}
	if got := ag.Usage(); got != snapshot.Usage {
		t.Fatalf("resumed memory usage = %+v, want %+v", got, snapshot.Usage)
	}
}

type failingCommitStore struct{ session.Store }

func (s failingCommitStore) Commit(session.Cursor, session.Batch) (session.CommitResult, error) {
	return session.CommitResult{}, session.ErrPersistenceFailed
}

func TestAgentSaveStopsAfterDurableWriteFailure(t *testing.T) {
	cfg := testPersistenceConfig(t)
	ag, err := New(&cfg, make(chan Event, 8))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ag.closePersistence()
		ag.Close()
	}()
	p := ag.persistenceHandle()
	if p == nil {
		t.Fatal("persistence missing")
	}
	p.mu.Lock()
	underlying := p.store
	p.store = failingCommitStore{Store: underlying}
	p.mu.Unlock()
	message := messages.Message{Role: messages.RoleAssistant, Content: "must not be confirmed", CreatedAt: time.Unix(950, 0).UTC()}
	if err := ag.appendHistory(message); !errors.Is(err, session.ErrPersistenceFailed) {
		t.Fatalf("appendHistory error = %v, want ErrPersistenceFailed", err)
	}
	if got := ag.History(); len(got) != 0 {
		t.Fatalf("failed append was published to history: %+v", got)
	}
	if err := ag.persistenceFailure(); !errors.Is(err, session.ErrPersistenceFailed) {
		t.Fatalf("persistence failure = %v", err)
	}
	receipt := ag.applySubmitInput(protocol.Command{
		ID:        "after-persistence-failure",
		SessionID: protocol.SessionID(ag.SessionID()),
		Type:      protocol.CommandSubmitInput,
		Input:     &protocol.SubmitInput{ID: "after-persistence-failure-input", Text: "must not run", Strategy: protocol.InputFollowup},
	})
	if !receipt.Rejected() || receipt.Error == nil {
		t.Fatalf("provider admission after persistence failure = %+v, want rejection", receipt)
	}
	if err := ag.Save(); !errors.Is(err, session.ErrPersistenceFailed) {
		t.Fatalf("second Save error = %v, want poisoned writer", err)
	}
}
