package journal

import (
	"ccdp/internal/session/replay"
	"errors"
	"math"
	"sync"
	"testing"

	"ccdp/internal/session"
)

func testWriter(t *testing.T) *Writer {
	t.Helper()
	w, err := Open(OpenOptions{SessionID: "writer-test", NoSessionPersistence: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func finishedAttempt() session.Event {
	return session.AttemptFinished{Attempt: session.Attempt{AttemptID: "attempt", RequestID: "request", Provider: "test", Model: "test", Outcome: "success", Usage: session.Usage{InputTokens: 1, TotalTokens: 1}}}
}

func TestCommitRebuildsFailedProjectionBeforeReturning(t *testing.T) {
	w := testWriter(t)
	// Simulate a corrupt in-memory cache, while the authoritative log is valid.
	w.projection.Usage.InputTokens = math.MaxInt64
	result, err := w.CommitEvents("attempt", finishedAttempt())
	if err != nil || !result.Applied || w.Failure() != nil {
		t.Fatalf("rebuild failed: %+v, %v", result, err)
	}
	if w.projection.Usage.InputTokens != 1 || w.projection.Usage.TurnCount != 1 || w.projectionCursor != result.Cursor {
		t.Fatalf("rebuild did not restore the committed state: %+v", w.projection.Usage)
	}
	_, err = w.CommitEvents("later", session.UsageChanged{Usage: session.Usage{InputTokens: 2, TotalTokens: 2, TurnCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	current := w.CurrentCursor()
	retry, err := w.CommitEvents("attempt", finishedAttempt())
	if err != nil || retry.Applied || w.CurrentCursor() != current || w.projection.Usage.InputTokens != 2 {
		t.Fatalf("retry changed cursor or applied facts twice: %+v, %v", retry, err)
	}
}

func TestUnprojectableCommitRetainsOutcomeAndBlocksFurtherWrites(t *testing.T) {
	w := testWriter(t)
	if _, err := w.CommitEvents("full", session.UsageChanged{Usage: session.Usage{InputTokens: math.MaxInt64, TotalTokens: math.MaxInt64}}); err != nil {
		t.Fatal(err)
	}
	result, err := w.CommitEvents("overflow", finishedAttempt())
	if !result.Applied || !errors.Is(err, session.ErrPersistenceFailed) || !errors.Is(w.Failure(), session.ErrPersistenceFailed) {
		t.Fatalf("lost committed outcome or failed to block admission: %+v, %v", result, err)
	}
	records, readErr := w.Read(session.Beginning)
	if readErr != nil || records[len(records)-1].Seq != result.Cursor || w.CurrentCursor() != result.Cursor {
		t.Fatalf("committed fact is not available for diagnosis: %v", readErr)
	}
	rejected, err := w.CommitEvents("later", session.UsageChanged{Usage: session.Usage{}})
	if rejected.Applied || !errors.Is(err, session.ErrPersistenceFailed) || w.CurrentCursor() != result.Cursor {
		t.Fatalf("unreconciled writer accepted another operation: %+v, %v", rejected, err)
	}
}

func TestBoundOwnerStateRemainsCanonicalAcrossRebuild(t *testing.T) {
	w := testWriter(t)
	var state replay.State
	var lock sync.Mutex
	if err := w.BindState(&state, &lock); err != nil {
		t.Fatal(err)
	}
	if w.projection != &state {
		t.Fatal("writer retained a second projection")
	}
	state.Usage.InputTokens = math.MaxInt64 // simulate memory corruption; log remains valid
	if _, err := w.CommitEvents("attempt", finishedAttempt()); err != nil {
		t.Fatal(err)
	}
	if w.projection != &state || state.Usage.InputTokens != 1 || state.Usage.TurnCount != 1 {
		t.Fatalf("rebuild detached owner state: %+v", state.Usage)
	}
	if _, err := w.CommitEvents("attempt", finishedAttempt()); err != nil {
		t.Fatal(err)
	}
	if state.Usage.InputTokens != 1 {
		t.Fatal("duplicate advanced canonical state")
	}
}
