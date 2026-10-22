package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMarkdownRoundTripDedupAndIndependentEdits(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	e, err := s.Update(ctx, Entry{Body: "默认中文回答。\n使用 pnpm。", Kind: "feedback", SourceSession: "a", SourceTurn: "turn-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, e.ID+".md"))
	if err != nil || !strings.Contains(string(raw), "\n\n默认中文回答。") || strings.Contains(string(raw), "revision") {
		t.Fatalf("Markdown: %s / %v", raw, err)
	}
	duplicate, err := s.Update(ctx, Entry{ID: "other", Body: e.Body, Kind: e.Kind, SourceSession: "b", SourceTurn: "turn-2"}, "")
	if err != nil || duplicate.ID != e.ID || len(duplicate.Sources) != 2 {
		t.Fatalf("dedup: %+v %v", duplicate, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, edit := range [][2]string{{"中文", "英文"}, {"pnpm", "bun"}} {
		wg.Add(1)
		go func(old, next string) { defer wg.Done(); _, err := s.Edit(ctx, e.ID, old, next, Entry{}); errs <- err }(edit[0], edit[1])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	e, err = s.Read(e.ID)
	if err != nil || e.Body != "默认英文回答。\n使用 bun。" {
		t.Fatalf("lost concurrent edit: %+v %v", e, err)
	}
	if _, err := s.Edit(ctx, e.ID, "pnpm", "npm", Entry{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit accepted: %v", err)
	}
}

func TestExternalEditsAndStaleIndexCannotHideCorrection(t *testing.T) {
	s := testStore(t, false)
	e, err := s.Update(context.Background(), Entry{ID: "language", Body: "Use Chinese."}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.Body = "Use English."
	raw, _ := encodeEntry(e)
	if err := os.WriteFile(filepath.Join(s.dir, "language.md"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "MEMORY.md"), []byte("Use Chinese."), 0600); err != nil {
		t.Fatal(err)
	}
	index, err := s.Index()
	if err != nil || !strings.Contains(index, "Use English.") || strings.Contains(index, ": Use Chinese.") {
		t.Fatalf("stale preview: %s %v", index, err)
	}
	// Plain Markdown created by the user is readable without metadata.
	if err := os.WriteFile(filepath.Join(s.dir, "manual.md"), []byte("# Tests\nRun the integration tests locally."), 0600); err != nil {
		t.Fatal(err)
	}
	if e, err := s.Read("manual"); err != nil || e.Title != "Tests" {
		t.Fatalf("plain Markdown: %+v %v", e, err)
	}
}

func TestHistoryRetryDeletionAndForegroundWrites(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	job, err := s.BeginHistory(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginHistory(ctx, "session-2"); !errors.Is(err, ErrHistoryBusy) {
		t.Fatalf("parallel history writer: %v", err)
	}
	entry := HistoryNote{Kind: "feedback", Body: "Use pnpm.", Turn: "turn-1"}
	if n, err := applyHistoryForTest(t, s, "session-1", entry); n != 1 || err != nil {
		t.Fatalf("import: %v %v", n, err)
	}
	// Simulate a crash after the note commit but before the progress commit.
	job.Close()
	if n, err := applyHistoryForTest(t, s, "session-1", entry); n != 0 || err != nil {
		t.Fatalf("retry duplicated note: %v %v", n, err)
	}
	job, err = s.BeginHistory(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if job.Cursor != 0 {
		t.Fatal("progress advanced before success")
	}
	entries, _ := s.List()
	id := entries[0].ID
	// The extraction lock does not block ordinary writes or deletion.
	if _, err := s.Edit(ctx, id, "pnpm", "bun", Entry{SourceSession: "session-2", SourceTurn: "turn-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, err := applyHistoryForTest(t, s, "session-1", entry); n != 0 || err == nil {
		t.Fatalf("forgotten source reimported: %v %v", n, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.dir, id+".md"))
	if strings.Contains(string(raw), "pnpm") || strings.Contains(string(raw), "bun") {
		t.Fatalf("forgotten content retained: %s", raw)
	}
	if err := job.Advance(42); err != nil {
		t.Fatal(err)
	}
}

func TestUserMemorySharedAndProjectMemorySeparated(t *testing.T) {
	base := t.TempDir()
	user, err := OpenUser(base, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := user.Update(context.Background(), Entry{ID: "language", Body: "Respond in Chinese.", Kind: "user"}, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		project, err := Open(base, t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		if entries, err := project.List(); err != nil || len(entries) != 0 {
			t.Fatalf("scope leakage: %+v %v", entries, err)
		}
		reopened, err := OpenUser(base, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.Read("language"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryModesHaveIndependentProgress(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	auto, err := s.BeginAutomaticHistory(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := auto.Advance(100); err != nil {
		t.Fatal(err)
	}
	auto.Close()
	manual, err := s.BeginHistory(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if manual.Cursor != 0 {
		t.Fatal("automatic skip hid history from explicit extraction")
	}
	manual.Close()
	auto, err = s.BeginAutomaticHistory(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	if auto.Cursor != 100 {
		t.Fatal("automatic progress lost after reopen")
	}
}

func TestDeleteCanRecoverAnOverfullDirectory(t *testing.T) {
	s := testStore(t, false)
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEntries+2; i++ {
		if err := os.WriteFile(filepath.Join(s.dir, fmt.Sprintf("note-%d.md", i)), []byte("An external note."), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(context.Background(), "note-0"); err != nil {
		t.Fatalf("cannot recover full directory: %v", err)
	}
	if _, err := s.Read("note-0"); !os.IsNotExist(err) {
		t.Fatal("deleted note readable")
	}
}
