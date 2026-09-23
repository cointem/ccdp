package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

// These tests exercise the production compaction/fork paths with a real
// Agent, then fail only the RequestPrepared fact. The provider must not run,
// and neither operation may take its old best-effort mutation path.
func TestCompactJournalAdmissionFailureLeavesHistoryUntouched(t *testing.T) {
	provider := &requestJournalGateProvider{}
	a := newJournalRuntimeAgent(t, provider)
	a.cfg.KeepAfterCompact = 4
	for i := 0; i < 7; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "history entry"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	before := a.History()

	installRequestPreparedFailure(t, a)
	a.compactContext(context.Background())

	if got := a.History(); !reflect.DeepEqual(got, before) {
		t.Fatalf("compaction changed history after journal failure:\n got=%+v\nwant=%+v", got, before)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider ran after RequestPrepared failure: %d calls", got)
	}
	if !errors.Is(a.persistenceFailure(), session.ErrPersistenceFailed) {
		t.Fatalf("journal failure was not retained: %v", a.persistenceFailure())
	}
}

func TestForkJournalAdmissionFailureDoesNotCreateChild(t *testing.T) {
	provider := &requestJournalGateProvider{}
	a := newJournalRuntimeAgent(t, provider)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "abandoned"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	before, _, err := ListSessions(a.cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}

	installRequestPreparedFailure(t, a)
	_, err = a.Fork(1)
	var journalErr *requestJournalFailure
	if err == nil || !errors.As(err, &journalErr) {
		t.Fatalf("fork error = %v, want request journal failure", err)
	}
	after, _, err := ListSessions(a.cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("fork created session after journal failure: before=%d after=%d", len(before), len(after))
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider ran after RequestPrepared failure: %d calls", got)
	}
}

func installRequestPreparedFailure(t *testing.T, a *Agent) {
	t.Helper()
	p := a.persistenceHandle()
	if p == nil {
		t.Fatal("agent persistence is unavailable")
	}
	p.mu.Lock()
	p.store = requestJournalFailEventStore{Store: p.store, event: session.EventTypeRequestPrepared}
	p.mu.Unlock()
}
