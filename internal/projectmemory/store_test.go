package projectmemory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func testStore(t *testing.T, readOnly bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "projects"), t.TempDir(), readOnly)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadsDoNotCreateFiles(t *testing.T) {
	s := testStore(t, false)
	if entries, err := s.List(); err != nil || len(entries) != 0 {
		t.Fatalf("%v %v", entries, err)
	}
	if _, err := os.Stat(s.base); !os.IsNotExist(err) {
		t.Fatalf("read created storage: %v", err)
	}
}

func TestDirectoryPreservesCanonicalProjectPath(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "工程 with spaces")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "projects")
	s, err := Open(base, workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectPath, err := filepath.Rel(string(filepath.Separator), canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.base, "workspace", projectPath, "memory")
	if s.Directory() != want {
		t.Fatalf("directory=%s, want readable path %s", s.Directory(), want)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	aliased, err := Open(base, alias, false)
	if err != nil || aliased.Directory() != want {
		t.Fatalf("workspace alias resolved to a different store: %+v %v", aliased, err)
	}
	if _, err := os.Stat(s.base); !os.IsNotExist(err) {
		t.Fatalf("opening store created directories: %v", err)
	}
}

func TestExactTextAndConcurrentWriters(t *testing.T) {
	s := testStore(t, false)
	e, err := s.Update(context.Background(), Entry{ID: "build", Title: "Build", Summary: "Build command", Body: "go test ./...", SourceSession: "session-a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			next := e
			next.Body = body
			_, err := s.Update(context.Background(), next, e.Body)
			out <- err
		}(body)
	}
	wg.Wait()
	close(out)
	success, conflict := 0, 0
	for err := range out {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	next, err := s.Read("build")
	if err != nil || next.Body == e.Body {
		t.Fatalf("%+v %v", next, err)
	}
	if err = s.Delete(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read("build"); !os.IsNotExist(err) {
		t.Fatalf("deleted note readable: %v", err)
	}
	if _, err = s.Update(context.Background(), Entry{ID: "build", Body: "replacement"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(context.Background(), e, e.Body); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale overwrite after recreation: %v", err)
	}

}

func TestReadOnlyAndConfinedStorage(t *testing.T) {
	s := testStore(t, true)
	_, err := s.Update(context.Background(), Entry{ID: "x", Title: "X", Body: "body"}, "")
	if !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err = s.Delete(context.Background(), "x"); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.base); !os.IsNotExist(err) {
		t.Fatalf("read-only mode created storage: %v", err)
	}
	s = testStore(t, false)
	if err = os.MkdirAll(filepath.Dir(filepath.Dir(s.dir)), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Dir(s.dir)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(context.Background(), Entry{ID: "x", Title: "X", Body: "body"}, "")
	if err == nil {
		t.Fatal("followed project directory symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("created files outside store")
	}
}

func TestIntermediateDirectorySymlinkIsRejected(t *testing.T) {
	s := testStore(t, false)
	// Even a redirect within the configured root must not alias another
	// project's memory when the mirrored path has several components.
	target := filepath.Join(s.base, "other-project")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.base, "workspace")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(context.Background(), Entry{ID: "x", Title: "X", Body: "body"}, ""); err == nil {
		t.Fatal("followed an intermediate storage directory symlink")
	}
	if _, err := s.List(); err == nil {
		t.Fatal("read through an intermediate storage directory symlink")
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("created files through redirect: %v %v", entries, err)
	}
}

func TestLimitsAndIndex(t *testing.T) {
	s := testStore(t, false)
	_, err := s.Update(context.Background(), Entry{ID: "too-big", Title: "Large", Body: strings.Repeat("x", MaxEntryBytes)}, "")
	if err == nil {
		t.Fatal("accepted oversized note")
	}
	e, err := s.Update(context.Background(), Entry{ID: "build", Title: "Build", Summary: "Run unit tests", Body: strings.Repeat("context ", 120) + "private long details"}, "")
	if err != nil {
		t.Fatal(err)
	}
	index, err := s.Index()
	if err != nil || !strings.Contains(index, "id: `"+e.ID+"`") || strings.Contains(index, e.ID+".md") || strings.Contains(index, "private long details") || len(index) > MaxIndexBytes {
		t.Fatalf("%q %v", index, err)
	}
	if _, err = s.Read("../build"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestLinkedWorktreeIdentity(t *testing.T) {
	repo := t.TempDir()
	git := filepath.Join(repo, ".git")
	worktreeGit := filepath.Join(git, "worktrees", "branch")
	if err := os.MkdirAll(worktreeGit, 0700); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+worktreeGit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGit, "commondir"), []byte("../..\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	a, err := Open(base, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(base, wt, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.dir != b.dir {
		t.Fatalf("different stores for worktrees: %s / %s", a.dir, b.dir)
	}
	canonicalGit, err := filepath.EvalSymlinks(git)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := filepath.Rel(string(filepath.Separator), canonicalGit)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(a.base, "git", gitPath, "memory"); a.dir != want {
		t.Fatalf("git directory=%s, want readable path %s", a.dir, want)
	}
	c, err := Open(base, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if a.dir == c.dir {
		t.Fatal("different project shares store")
	}
}

func TestSpecialFilesCannotBlockMemoryReadsOrWrites(t *testing.T) {
	for _, name := range []string{"note.md", ".lock"} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t, false)
			if err := os.MkdirAll(s.dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(s.dir, name), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if name == "note.md" {
				_, err = s.List()
			} else {
				_, err = s.Update(context.Background(), Entry{ID: "note", Title: "Note", Body: "text"}, "")
			}
			if err == nil {
				t.Fatal("accepted non-regular memory file")
			}
		})
	}
}
