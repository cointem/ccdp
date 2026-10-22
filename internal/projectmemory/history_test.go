package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func applyHistoryForTest(t *testing.T, s *Store, session string, notes ...HistoryNote) (int, error) {
	t.Helper()
	snapshot, err := s.HistorySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.ApplyHistory(context.Background(), snapshot, session, notes)
}

func TestHistoryReconcilesMultipleFactsAndPreservesEvidence(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	original, err := s.Update(ctx, Entry{ID: "tests", Kind: "feedback", Body: "Run integration tests locally.", SourceSession: "s", SourceTurn: "turn-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if HistoryExcluded([]Entry{original}, "s", "turn-1") {
		t.Fatal("ordinary save excluded unrelated evidence")
	}
	n, err := applyHistoryForTest(t, s, "s",
		HistoryNote{ID: "tests", Turn: "turn-1", Kind: "feedback", Body: "Run integration tests locally with Redis."},
		HistoryNote{Turn: "turn-1", Kind: "reference", Body: "Release checklist lives in the team wiki."},
		HistoryNote{Turn: "turn-2", Kind: "reference", Body: "Release checklist lives in the team wiki."},
	)
	if err != nil || n != 2 {
		t.Fatalf("apply=%d %v", n, err)
	}
	entries, _ := s.List()
	if len(entries) != 2 {
		t.Fatalf("entries=%+v", entries)
	}
	updated, _ := s.Read("tests")
	if updated.Body != "Run integration tests locally with Redis." || !updated.CreatedAt.Equal(original.CreatedAt) || len(updated.Sources) != 1 {
		t.Fatalf("update=%+v", updated)
	}
	for _, e := range entries {
		if e.ID != "tests" && len(e.Sources) != 2 {
			t.Fatalf("duplicate lost evidence: %+v", e)
		}
	}
	// A retry after a partial commit reconciles against the notes that survived.
	n, err = applyHistoryForTest(t, s, "s", HistoryNote{Turn: "turn-1", Kind: "reference", Body: "Release checklist lives in the team wiki."})
	if err != nil || n != 0 {
		t.Fatalf("retry=%d %v", n, err)
	}
}

func TestHistoryBatchRejectsInvalidProposalsBeforeWriting(t *testing.T) {
	for _, invalid := range []HistoryNote{
		{ID: "missing", Turn: "turn-1", Kind: "project", Body: "Unknown target"},
		{Turn: "turn-1", Kind: "project", Body: strings.Repeat("x", MaxEntryBytes)},
		{Turn: "turn-1", Kind: "user", Body: "User scope is not writable"},
	} {
		t.Run(invalid.ID+invalid.Kind, func(t *testing.T) {
			s := testStore(t, false)
			n, err := applyHistoryForTest(t, s, "s", HistoryNote{Turn: "turn-1", Kind: "project", Body: "Valid new fact"}, invalid)
			if err == nil || n != 0 {
				t.Fatalf("apply=%d %v", n, err)
			}
			entries, _ := s.List()
			if len(entries) != 0 {
				t.Fatalf("partial invalid batch: %+v", entries)
			}
		})
	}
}

func TestHistoryConcurrentChangesInvalidateSnapshot(t *testing.T) {
	for _, action := range []string{"edit", "replace", "delete", "add"} {
		t.Run(action, func(t *testing.T) {
			s := testStore(t, false)
			ctx := context.Background()
			_, err := s.Update(ctx, Entry{ID: "tool", Body: "Use pnpm.", SourceSession: "s", SourceTurn: "turn-1"}, "")
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.HistorySnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "edit":
				_, err = s.Edit(ctx, "tool", "pnpm", "bun", Entry{SourceSession: "s", SourceTurn: "turn-2"})
			case "replace":
				_, err = s.Update(ctx, Entry{ID: "tool", Body: "Use bun.", SourceSession: "s", SourceTurn: "turn-2"}, "Use pnpm.")
			case "delete":
				err = s.Delete(ctx, "tool")
			case "add":
				_, err = s.Update(ctx, Entry{Body: "Prefer local builds."}, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _ := s.HistorySnapshot(ctx)
			n, err := s.ApplyHistory(ctx, before, "s", []HistoryNote{{Turn: "turn-1", Kind: "project", Body: "Use pnpm for release."}})
			if !errors.Is(err, ErrConflict) || n != 0 {
				t.Fatalf("stale apply=%d %v", n, err)
			}
			after, _ := s.HistorySnapshot(ctx)
			if !reflect.DeepEqual(current, after) {
				t.Fatal("concurrent foreground change overwritten")
			}
			if action != "add" && !HistoryExcluded(after, "s", "turn-1") {
				t.Fatal("superseded source remains eligible")
			}
		})
	}
}

func TestHistoryRepeatedTargetDoesNotPartiallyUpdate(t *testing.T) {
	s := testStore(t, false)
	_, err := s.Update(context.Background(), Entry{ID: "topic", Body: "Original"}, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := applyHistoryForTest(t, s, "s",
		HistoryNote{ID: "topic", Turn: "turn-1", Kind: "project", Body: "First"},
		HistoryNote{ID: "topic", Turn: "turn-2", Kind: "project", Body: "Second"})
	if n != 0 || err == nil {
		t.Fatalf("apply=%d %v", n, err)
	}
	e, _ := s.Read("topic")
	if e.Body != "Original" {
		t.Fatalf("partial write: %+v", e)
	}
}

func TestHistoryKeepsLegacyCorrectionsAndReusedIDExcluded(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	e, err := s.Update(ctx, Entry{ID: "tool", Body: "Use bun.", SourceSession: "s", SourceTurn: "turn-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.UpdatedAt = e.CreatedAt.Add(time.Hour)
	raw, _ := encodeEntry(e)
	// An older corrected file has no excluded_sources field.
	raw = []byte(strings.Replace(string(raw), `  "excluded_sources": [],`+"\n", "", 1))
	if err := os.WriteFile(filepath.Join(s.dir, e.ID+".md"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.HistorySnapshot(ctx)
	if err != nil || !HistoryExcluded(snapshot, "s", "turn-1") {
		t.Fatalf("legacy correction lost: %+v %v", snapshot, err)
	}
	if err := s.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, Entry{ID: "tool", Body: "Use local Redis.", SourceSession: "s", SourceTurn: "turn-2"}, ""); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = s.HistorySnapshot(ctx)
	if !HistoryExcluded(snapshot, "s", "turn-1") || HistoryExcluded(snapshot, "s", "turn-2") {
		t.Fatalf("reused ID exclusions: %+v", snapshot)
	}
}

func TestHistoryTidyDoesNotTreatDuplicatesAsForgotten(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	e, err := s.Update(ctx, Entry{ID: "first", Body: "Use pnpm.", SourceSession: "s", SourceTurn: "turn-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.ID = "second"
	e.SourceTurn = "turn-2"
	e.Sources = []Source{{Session: "s", Turn: "turn-2"}}
	raw, _ := encodeEntry(e)
	if err := os.WriteFile(filepath.Join(s.dir, "second.md"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Tidy(ctx); err != nil || n != 1 {
		t.Fatalf("tidy=%d %v", n, err)
	}
	snapshot, _ := s.HistorySnapshot(ctx)
	if HistoryExcluded(snapshot, "s", "turn-1") || HistoryExcluded(snapshot, "s", "turn-2") {
		t.Fatal("dedup excluded unrelated facts")
	}
	if err := s.Delete(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = s.HistorySnapshot(ctx)
	if !HistoryExcluded(snapshot, "s", "turn-1") || !HistoryExcluded(snapshot, "s", "turn-2") {
		t.Fatal("forget failed to exclude merged evidence")
	}
}

func TestHistoryBatchChecksAggregateCapacityBeforeWriting(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	if _, err := s.Update(ctx, Entry{ID: "first", Body: "First note"}, ""); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxEntries-1; i++ {
		e := Entry{ID: fmt.Sprintf("note-%d", i), Body: fmt.Sprintf("Fact %d", i), Kind: "project"}
		raw, _ := encodeEntry(e)
		if err := os.WriteFile(filepath.Join(s.dir, e.ID+".md"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := applyHistoryForTest(t, s, "s", HistoryNote{Turn: "turn-1", Kind: "project", Body: "New fact one"}, HistoryNote{Turn: "turn-1", Kind: "project", Body: "New fact two"})
	if n != 0 || err == nil {
		t.Fatalf("overfull batch=%d %v", n, err)
	}
	entries, _ := s.List()
	if len(entries) != MaxEntries-1 {
		t.Fatalf("partially committed overfull batch: %d", len(entries))
	}
}

func TestHistoryForgetDoesNotDuplicateLargeProvenance(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	entry := Entry{ID: "lesson", Body: "Use local Redis."}
	for i := 0; i < 45; i++ {
		entry.Sources = append(entry.Sources, Source{Session: fmt.Sprintf("%s-%d", strings.Repeat("s", 40), i), Turn: "turn-1"})
	}
	saved, err := s.Update(ctx, entry, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := encodeEntry(saved)
	if len(raw) <= MaxEntryBytes/2 {
		t.Fatal("fixture must exceed half the per-entry limit")
	}
	if err := s.Delete(ctx, saved.ID); err != nil {
		t.Fatalf("forget failed on a valid note: %v", err)
	}
	snapshot, err := s.HistorySnapshot(ctx)
	if err != nil || !HistoryExcluded(snapshot, entry.Sources[0].Session, "turn-1") {
		t.Fatalf("forget provenance lost: %v", err)
	}
}
