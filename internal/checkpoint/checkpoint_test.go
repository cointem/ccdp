package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
