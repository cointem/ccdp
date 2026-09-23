package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// TestResumeRestoresRuntimeSequences exercises the durable boundary that was
// previously missing from Resume: the first post-resume turn must receive a
// fresh turn/step identity and remain writable.
func TestResumeRestoresRuntimeSequences(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	formalCompletionTurn(t, a, "bug-resume-first", "before resume")
	cfg := a.configSnapshot()
	sessionID := a.SessionID()
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionID = sessionID
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	b, err := newAgentWithOptions(&cfg, nil, snapshot, Options{
		EffectiveConfigFrozen: true,
		ProviderRegistry:      models,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseContext(context.Background())
	formalCompletionTurn(t, b, "bug-resume-second", "after resume")
	if provider.requestsCount() != 2 {
		t.Fatalf("post-resume provider calls=%d, want 2", provider.requestsCount())
	}
	if err := b.persistenceFailure(); err != nil {
		t.Fatalf("post-resume turn poisoned persistence: %v", err)
	}
}

// TestConsecutiveModelChangesLeaveNoStaleCandidate verifies that back-to-back
// model changes apply immediately (even while busy) and the last one wins, with
// no stale staged candidate surviving into replay.
func TestConsecutiveModelChangesLeaveNoStaleCandidate(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	a.models.Route("bug-first-model", provider.Name())
	a.models.Route("bug-second-model", provider.Name())
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy = false
		a.mu.Unlock()
	}()

	first := a.applyCommand(protocol.NewSetModel("bug-first-command", protocol.SessionID(a.SessionID()), "bug-first-model"))
	if first.Rejected() || first.Status != protocol.ReceiptApplied {
		t.Fatalf("first model receipt=%+v, want applied", first)
	}
	second := a.applyCommand(protocol.NewSetModel("bug-second-command", protocol.SessionID(a.SessionID()), "bug-second-model"))
	if second.Rejected() || second.Status != protocol.ReceiptApplied {
		t.Fatalf("second model receipt=%+v, want applied", second)
	}
	a.mu.Lock()
	activeModel := a.activeBinding.model
	a.mu.Unlock()
	if activeModel != "bug-second-model" {
		t.Fatalf("last model did not win cleanly: active=%q", activeModel)
	}
	replayed, err := SessionSnapshotProjection(a.persistenceHandle().Store(), a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Model != "bug-second-model" {
		t.Fatalf("replayed settings retained stale candidate: model=%q", replayed.Model)
	}
}

func TestToolProjectionUsesOccurrenceIdentityAndStablePrefix(t *testing.T) {
	records := []session.Record{
		{Seq: 1, Event: &session.ToolResultsProjected{TurnID: "turn-1", StepID: "step-1", Results: []session.ProjectedToolResult{{CallID: "reused", Text: "same", Status: "success"}}}},
		{Seq: 2, Event: &session.ToolResultsProjected{TurnID: "turn-1", StepID: "step-2", Results: []session.ProjectedToolResult{{CallID: "reused", Text: "same", Status: "success"}}}},
	}
	projection, err := projectRecordsRaw(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.History) != 2 {
		t.Fatalf("projected history=%+v, want two occurrences", projection.History)
	}
	if projection.History[0].ID == projection.History[1].ID {
		t.Fatalf("same call ID across steps reused message ID %q", projection.History[0].ID)
	}

	provider := &formalToolFactProvider{}
	a := newJournalRuntimeAgent(t, provider)
	a.registry.Register(&formalAuditEffect{t: t, a: a})
	submitJournalRuntimeInput(t, a)
	waitJournalRuntimeIdle(t, a)
	actual, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range actual {
		if record.Event.Type() == session.EventTypeConversationReset {
			t.Fatalf("normal tool turn rewrote conversation at seq %d", record.Seq)
		}
	}
}

// TestOversizedInputIsRetryableBeforePersistence confirms the protocol raw
// text bound rejects a user-correctable payload before it can enter the log,
// without latching the session.
func TestOversizedInputIsRetryableBeforePersistence(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	text := strings.Repeat("\x00", 1_500_000)
	large := protocol.NewSubmitInput("bug-large-input", protocol.SessionID(a.SessionID()), "bug-large-input-id", text, protocol.InputSteer)
	receipt, err := a.Submit(context.Background(), large)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
		t.Fatalf("oversized input receipt=%+v, want recoverable invalid command", receipt)
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("oversized input poisoned persistence: %v", err)
	}
	formalCompletionTurn(t, a, "bug-large-input-retry", "small retry")
	if provider.requestsCount() != 1 {
		t.Fatalf("retry provider calls=%d, want 1", provider.requestsCount())
	}
}

type inputTooLargeCommitStore struct {
	session.Store
	once sync.Once
}

func (s *inputTooLargeCommitStore) Commit(cursor session.Cursor, batch session.Batch) (session.CommitResult, error) {
	for _, event := range batch.Events {
		if event.Type() == session.EventTypeInputQueued {
			injected := false
			s.once.Do(func() { injected = true })
			if injected {
				return session.CommitResult{}, fmt.Errorf("%w: injected InputQueued transaction limit", session.ErrTooLarge)
			}
			break
		}
	}
	return s.Store.Commit(cursor, batch)
}

// TestInputQueuedTooLargeIsRecoverable exercises the store-side ErrTooLarge
// branch separately from Normalize's raw protocol limit. A valid later
// Submit must still reach the provider, proving the cache/admission error did
// not poison the durable writer.
func TestInputQueuedTooLargeIsRecoverable(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	p := a.persistenceHandle()
	p.mu.Lock()
	p.store = &inputTooLargeCommitStore{Store: p.store}
	p.mu.Unlock()
	command := protocol.NewSubmitInput("bug-store-input-too-large", protocol.SessionID(a.SessionID()), "bug-store-input-too-large-id", "valid text", protocol.InputSteer)
	receipt, err := a.Submit(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
		t.Fatalf("store ErrTooLarge receipt=%+v, want recoverable invalid command", receipt)
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("store ErrTooLarge poisoned persistence: %v", err)
	}
	formalCompletionTurn(t, a, "bug-store-input-too-large-retry", "valid retry after store admission limit")
	if provider.requestsCount() != 1 {
		t.Fatalf("provider calls after store ErrTooLarge=%d, want 1", provider.requestsCount())
	}
}

// TestMaximumEscapedInputDoesNotPoisonOnSnapshotOverflow covers the boundary
// where a 1 MiB protocol input is valid, but the optional compatibility
// snapshot becomes larger than the JSONL transaction limit after JSON
// escaping. The typed event log must remain writable and sufficient for a
// complete resume even when snapshot.json is skipped.
func TestMaximumEscapedInputDoesNotPoisonOnSnapshotOverflow(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newLargeContextJournalRuntimeAgent(t, provider)
	text := strings.Repeat("\x01", protocol.MaxSubmitInputTextBytes)
	command := protocol.NewSubmitInput("bug-max-escaped-input", protocol.SessionID(a.SessionID()), "bug-max-escaped-input-id", text, protocol.InputSteer)
	receipt, err := a.Submit(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Rejected() {
		t.Fatalf("maximum escaped input rejected: %+v", receipt)
	}
	view := waitJournalRuntimeIdle(t, a)
	if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("maximum escaped input turn=%+v", view.LastTurn)
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("snapshot overflow poisoned typed log: %v", err)
	}
	formalCompletionTurn(t, a, "bug-max-escaped-retry", "valid retry after snapshot overflow")
	if provider.requestsCount() != 2 {
		t.Fatalf("post-overflow provider calls=%d, want 2", provider.requestsCount())
	}

	cfg := a.configSnapshot()
	sessionID := a.SessionID()
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range snapshot.History {
		if message.Content == text {
			found = true
			break
		}
	}
	if len(snapshot.History) < 4 || !found {
		t.Fatalf("replayed log lost maximum input: history=%d found=%v", len(snapshot.History), found)
	}
}

// TestApplyPlanTextLimitIsRecoverable keeps the separate 16 MiB patch-file
// admission bound, but verifies that a textual /apply plan is still subject
// to SubmitInput's 1 MiB protocol bound when it is converted to a user turn.
func TestApplyPlanTextLimitIsRecoverable(t *testing.T) {
	provider := &formalCompletionProvider{}
	a := newJournalRuntimeAgent(t, provider)
	planPath := filepath.Join(a.cfg.Workspace, "large-plan.txt")
	if err := os.WriteFile(planPath, []byte(strings.Repeat("plan ", 2<<20/len("plan "))), 0o600); err != nil {
		t.Fatal(err)
	}
	command := protocol.Command{ID: "bug-large-apply", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApply,
		Apply: &protocol.ApplyCommand{Path: planPath}}
	receipt, err := a.Submit(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Rejected() {
		t.Fatalf("large textual apply was rejected before its operation boundary: %+v", receipt)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		view, snapshotErr := a.Snapshot(context.Background())
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if !view.Busy {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("large textual apply operation did not settle")
		case <-time.After(time.Millisecond):
		}
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("recoverable textual apply rejection poisoned persistence: %v", err)
	}
	formalCompletionTurn(t, a, "bug-large-apply-retry", "valid submit after large apply")
	if provider.requestsCount() != 1 {
		t.Fatalf("provider calls after large apply=%d, want valid retry only", provider.requestsCount())
	}
}

func TestIdempotencyIndexesRetainDigestsNotPayloads(t *testing.T) {
	a := newJournalRuntimeAgent(t, &formalCompletionProvider{})
	command := protocol.Command{ID: "bug-digest-command", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandInterrupt}
	normalized, err := command.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	receipt := a.applyCommand(normalized)
	if receipt.Rejected() {
		t.Fatalf("interrupt command rejected: %+v", receipt)
	}
	commandDigest, err := commandInputDigest(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.seenCommands[normalized.ID]; got != commandDigest || len(got) != 64 {
		t.Fatalf("seen command index=%q, want canonical digest %q", got, commandDigest)
	}

	input := protocol.SubmitInput{ID: "bug-digest-input", Text: "payload that must not remain in the index", Strategy: protocol.InputFollowup}
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	inputCommand := protocol.Command{ID: "bug-digest-input-command", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSubmitInput, Input: &input}
	inputReceipt := a.applySubmitInput(inputCommand)
	if inputReceipt.Rejected() {
		t.Fatalf("input admission rejected: %+v", inputReceipt)
	}
	inputDigest, err := submitInputDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.seenInputBody[input.ID]; got != inputDigest || len(got) != 64 || strings.Contains(got, input.Text) {
		t.Fatalf("seen input index=%q, want canonical digest %q", got, inputDigest)
	}
	a.mu.Lock()
	a.busy = false
	a.mu.Unlock()
}

func newLargeContextJournalRuntimeAgent(t *testing.T, provider llm.Provider) *Agent {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.Model = "request-journal-gate-model"
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.MaxTurns = 2
	cfg.ContextWindow = 1_000_000
	registry := plugin.NewModelRegistry()
	registry.Register(provider)
	registry.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
	return a
}

func TestAutomaticCompactionFailureIsSuppressedButManualRetries(t *testing.T) {
	provider := &formalCompletionProvider{fail: true}
	a := newJournalRuntimeAgent(t, provider)
	a.cfg.KeepAfterCompact = 4
	for i := 0; i < 7; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: fmt.Sprintf("compact entry %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before := a.History()
	a.compact()
	if got := provider.requestsCount(); got != 1 {
		t.Fatalf("automatic compaction attempts=%d, want 1", got)
	}
	a.compact()
	if got := provider.requestsCount(); got != 1 {
		t.Fatalf("same-state automatic retry attempts=%d, want suppressed at 1", got)
	}
	a.mu.Lock()
	failureRecorded := a.compactFailure != nil
	a.mu.Unlock()
	if !failureRecorded {
		t.Fatal("automatic compaction failure was not recorded")
	}
	// The explicit/manual path bypasses the automatic suppression and still
	// leaves the original history untouched when the provider keeps failing.
	a.compactContext(context.Background())
	if got := provider.requestsCount(); got != 2 {
		t.Fatalf("manual compaction attempts=%d, want retry at 2", got)
	}
	if got := a.History(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed compaction changed history:\n got=%+v\nwant=%+v", got, before)
	}
}

type approvalResolutionBarrierStore struct {
	session.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *approvalResolutionBarrierStore) Commit(cursor session.Cursor, batch session.Batch) (session.CommitResult, error) {
	result, err := s.Store.Commit(cursor, batch)
	for _, event := range batch.Events {
		if event.Type() == session.EventTypeApprovalResolved {
			s.once.Do(func() {
				close(s.entered)
				<-s.release
			})
			break
		}
	}
	return result, err
}

// TestApprovalTimeoutAndAllowCommitExactlyOneFact forces the timeout/user
// race at the durable-resolution boundary. Whichever path claims the
// resolution first must be the only path that commits ApprovalResolved.
func TestApprovalTimeoutAndAllowCommitExactlyOneFact(t *testing.T) {
	a := newRuntimeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := a.persistenceHandle()
	a.mu.Lock()
	a.turnCtx = ctx
	a.turnCancel = cancel
	a.mu.Unlock()
	if p == nil {
		t.Fatal("session persistence missing")
	}
	barrier := &approvalResolutionBarrierStore{Store: p.Store(), entered: make(chan struct{}), release: make(chan struct{})}
	p.mu.Lock()
	p.store = barrier
	p.mu.Unlock()

	tc := messages.ToolCall{ID: "approval-race-call", Name: "Bash", Arguments: map[string]any{"command": "echo safe"}}
	resultCh := make(chan struct {
		approved bool
		remember bool
	}, 1)
	go func() {
		approved, remember := a.requestApproval(tc, "race test")
		resultCh <- struct {
			approved bool
			remember bool
		}{approved: approved, remember: remember}
	}()

	var approval *ApprovalRequest
	deadline := time.After(3 * time.Second)
	for approval == nil {
		select {
		case event := <-a.events:
			if event.Type == EventApproval && event.Approval != nil {
				approval = event.Approval
			}
		case <-deadline:
			t.Fatal("approval request was not published")
		}
	}
	commandResult := make(chan protocol.Receipt, 1)
	go func() {
		commandResult <- a.applyApproveTool(protocol.Command{
			ID:        "approval-race-command",
			SessionID: protocol.SessionID(a.SessionID()),
			Type:      protocol.CommandApproveTool,
			Approval:  &protocol.ApproveTool{ApprovalID: approval.ID, Approve: true},
		})
	}()
	select {
	case <-barrier.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval resolution did not reach commit barrier")
	}
	// The command has claimed the resolution and is blocked in Commit. The
	// timeout path must wait for its answer instead of writing a cancel fact.
	cancel()
	close(barrier.release)
	select {
	case receipt := <-commandResult:
		if receipt.Rejected() {
			t.Fatalf("allow command rejected: %+v", receipt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("allow command did not finish")
	}
	select {
	case outcome := <-resultCh:
		if !outcome.approved {
			t.Fatalf("approval result=%+v", outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval waiter did not finish")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, record := range records {
		if record.Event.Type() == session.EventTypeApprovalResolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("ApprovalResolved facts=%d, want exactly one", resolved)
	}
}

func TestMemoryCacheFailureDoesNotPoisonTypedMemory(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	if err := os.MkdirAll(a.memoryFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.turnUserMsg = "cache failure request"
	a.turnSummary = "typed memory remains authoritative"
	a.mu.Unlock()
	if err := a.recordMemory(); err != nil {
		t.Fatalf("recordMemory returned cache failure: %v", err)
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("cache failure poisoned persistence: %v", err)
	}
	if got := a.MemoryText(); !strings.Contains(got, "cache failure request") {
		t.Fatalf("typed memory was not retained after cache failure: %q", got)
	}
	if p := a.persistenceHandle(); p != nil {
		records, err := p.Read(session.Beginning)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, record := range records {
			if record.Event.Type() == session.EventTypeMemoryChanged {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("typed MemoryChanged fact missing after cache failure")
		}
	}
}
