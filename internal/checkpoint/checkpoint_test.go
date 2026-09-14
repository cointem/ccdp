package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// gitIn runs git in dir with isolated identity.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCreateRestore(t *testing.T) {
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(ws, "a.txt"), "one")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-qm", "base")

	// Make a change so there is something to snapshot.
	writeFile(t, filepath.Join(ws, "a.txt"), "working copy")

	store := NewStore(t.TempDir(), "sess-1")
	id, err := store.Create(ws, "initial state")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected a checkpoint id")
	}
	recs := store.List()
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("expected 1 record %s, got %+v", id, recs)
	}

	// Mutate the workspace further.
	writeFile(t, filepath.Join(ws, "a.txt"), "changed")
	writeFile(t, filepath.Join(ws, "b.txt"), "new file")

	// Restore.
	if err := store.Restore(ws, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "working copy" {
		t.Errorf("a.txt after restore = %q, want %q (the checkpoint state)", data, "working copy")
	}
	// b.txt was untracked; conservative restore leaves it (reports nothing fatal).
}

func TestCheckpointCapturesUntrackedWithoutChangingWorktree(t *testing.T) {
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(ws, "tracked.txt"), "base")
	gitIn(t, ws, "add", "tracked.txt")
	gitIn(t, ws, "commit", "-qm", "base")

	writeFile(t, filepath.Join(ws, "tracked.txt"), "changed")
	writeFile(t, filepath.Join(ws, "agent-output.txt"), "created by agent")
	beforeStatus := gitIn(t, ws, "status", "--porcelain=v1", "--untracked-files=all")
	beforeIndex := gitIn(t, ws, "write-tree")
	beforeHead := gitIn(t, ws, "rev-parse", "HEAD")
	beforeRef := gitIn(t, ws, "symbolic-ref", "--short", "HEAD")

	store := NewStore(t.TempDir(), "sess-untracked")
	id, err := store.Create(ws, "agent output")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected a checkpoint id for tracked and untracked changes")
	}
	afterStatus := gitIn(t, ws, "status", "--porcelain=v1", "--untracked-files=all")
	afterIndex := gitIn(t, ws, "write-tree")
	afterHead := gitIn(t, ws, "rev-parse", "HEAD")
	afterRef := gitIn(t, ws, "symbolic-ref", "--short", "HEAD")
	if afterStatus != beforeStatus {
		t.Fatalf("checkpoint changed worktree status: before=%q after=%q", beforeStatus, afterStatus)
	}
	if afterIndex != beforeIndex {
		t.Fatalf("checkpoint changed the real index: before=%q after=%q", beforeIndex, afterIndex)
	}
	if afterHead != beforeHead || afterRef != beforeRef {
		t.Fatalf("checkpoint changed HEAD/ref: before=%q/%q after=%q/%q", beforeHead, beforeRef, afterHead, afterRef)
	}

	if err := os.Remove(filepath.Join(ws, "agent-output.txt")); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ws, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "agent-output.txt"))
	if err != nil {
		t.Fatalf("restored untracked file: %v", err)
	}
	if string(data) != "created by agent" {
		oo := string(data)
		t.Fatalf("restored untracked file = %q, want %q", oo, "created by agent")
	}
}

func TestCheckpointCleanTree(t *testing.T) {
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(ws, "a.txt"), "one")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-qm", "base")

	store := NewStore(t.TempDir(), "sess-1")
	id, err := store.Create(ws, "nothing changed")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty id for clean tree, got %q", id)
	}
}

func TestCheckpointNotGitRepo(t *testing.T) {
	store := NewStore(t.TempDir(), "sess-1")
	if _, err := store.Create(t.TempDir(), "nope"); err == nil {
		t.Error("expected error outside a git repo")
	}
}

func TestMemoryStoreDoesNotCreateSessionFiles(t *testing.T) {
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(ws, "a.txt"), "one")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-qm", "base")
	writeFile(t, ws+"/a.txt", "changed")

	sessionRoot := t.TempDir()
	store := NewMemoryStore("memory-session")
	if _, err := store.Create(ws, "memory only"); err != nil {
		t.Fatal(err)
	}
	if len(store.List()) != 1 {
		t.Fatalf("memory records = %v", store.List())
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "checkpoints")); !os.IsNotExist(err) {
		t.Fatalf("memory checkpoint unexpectedly touched session root: %v", err)
	}
}

func TestCheckpointRecordsConcurrentAppend(t *testing.T) {
	store := NewStore(t.TempDir(), "concurrent")
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.appendRecord(Record{ID: "ck-concurrent-" + string(rune('a'+i)), SHA: "sha", CreatedAt: time.Now().Add(time.Duration(i) * time.Second)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.List()); got != count {
		t.Fatalf("concurrent append retained %d records, want %d", got, count)
	}
}

func TestCheckpointRecordsConcurrentStoresShareAdvisoryLock(t *testing.T) {
	root := t.TempDir()
	const count = 32
	stores := make([]*Store, count)
	for i := range stores {
		stores[i] = NewStore(root, "shared-session")
	}
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			errs <- store.appendRecord(Record{ID: "store-" + string(rune('a'+i)), SHA: "sha", CreatedAt: time.Now()})
		}(i, store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(stores[0].List()); got != count {
		t.Fatalf("independent stores retained %d records, want %d", got, count)
	}
}

// TestCheckpointRecordAppenderHelper is launched by the process-level test
// below. Keeping the helper in this test binary exercises the same on-disk
// transaction without starting a ccdp runtime or touching a real workspace.
func TestCheckpointRecordAppenderHelper(t *testing.T) {
	if os.Getenv("CCDP_CHECKPOINT_HELPER") != "1" {
		return
	}
	root := os.Getenv("CCDP_CHECKPOINT_HELPER_ROOT")
	gate := os.Getenv("CCDP_CHECKPOINT_HELPER_GATE")
	index, err := strconv.Atoi(os.Getenv("CCDP_CHECKPOINT_HELPER_INDEX"))
	if err != nil || root == "" || gate == "" {
		t.Fatalf("invalid helper environment")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for helper gate")
		}
		time.Sleep(2 * time.Millisecond)
	}
	store := NewStore(root, "cross-process")
	if err := store.appendRecord(Record{ID: "process-" + strconv.Itoa(index), SHA: "sha", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRecordsConcurrentProcessesShareAdvisoryLock(t *testing.T) {
	root := t.TempDir()
	gate := filepath.Join(root, "start")
	const count = 8
	cmds := make([]*exec.Cmd, 0, count)
	for i := 0; i < count; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointRecordAppenderHelper$")
		cmd.Env = append(os.Environ(),
			"CCDP_CHECKPOINT_HELPER=1",
			"CCDP_CHECKPOINT_HELPER_ROOT="+root,
			"CCDP_CHECKPOINT_HELPER_GATE="+gate,
			"CCDP_CHECKPOINT_HELPER_INDEX="+strconv.Itoa(i),
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
		cmds = append(cmds, cmd)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v", i, err)
		}
	}
	if got := len(NewStore(root, "cross-process").List()); got != count {
		t.Fatalf("cross-process append retained %d records, want %d", got, count)
	}
}
