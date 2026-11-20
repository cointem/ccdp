package desktop

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/atomicfile"
)

func TestProjectStoreKeepsEmptyProjectsAndResolvesAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs", "desktop-projects.json")
	read := func() *projectStore {
		t.Helper()
		s, err := loadProjectStore(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := filepath.Join(dir, "team", "app")
	second := filepath.Join(dir, "personal", "app")
	for _, folder := range []string{first, second} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "shortcut")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}

	s := read()
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading created a preferences directory: %v", err)
	}
	if err := s.update(link, func(m *projectMetadata) {
		m.Name, m.Pinned, m.Collapsed = " 工作项目  ", true, true
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.update(first, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.update(second, nil); err != nil {
		t.Fatal(err)
	}
	s = read()
	want := projectMetadata{Name: "工作项目", Pinned: true, Collapsed: true}
	if len(s.Items) != 2 || s.get(link) != want || s.get(first) != want {
		t.Fatalf("project identity or persistence lost: %+v", s.Items)
	}
	if item, ok := s.Items[projectKey(second)]; !ok || item != (projectMetadata{}) {
		t.Fatal("empty second project with the same basename was lost")
	}
}

func TestProjectStoreMergesChangesAndProtectsDamagedData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	first, err := loadProjectStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadProjectStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.update(dir, func(m *projectMetadata) { m.Name = "本地项目" }); err != nil {
		t.Fatal(err)
	}
	if err := second.update(dir, func(m *projectMetadata) { m.Pinned = true }); err != nil {
		t.Fatal(err)
	}
	if err := first.update(dir, func(m *projectMetadata) { m.Collapsed = true }); err != nil {
		t.Fatal(err)
	}
	want := projectMetadata{Name: "本地项目", Pinned: true, Collapsed: true}
	if got := first.get(dir); got != want {
		t.Fatalf("update overwrote a stale window's preferences: %+v", got)
	}
	lock, err := atomicfile.AcquireLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	err = first.update(dir, func(m *projectMetadata) { m.Pinned = false })
	lock.Close()
	if !errors.Is(err, atomicfile.ErrLocked) || first.get(dir) != want {
		t.Fatalf("lock contention changed data: %v", err)
	}
	const damaged = "{incomplete"
	if err := os.WriteFile(path, []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.update(dir, nil); err == nil || first.get(dir) != want {
		t.Fatal("could not preserve the last good snapshot on a read error")
	}
	if _, err := loadProjectStore(path); err == nil {
		t.Fatal("missing load error for damaged metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != damaged {
		t.Fatalf("damaged metadata overwritten: %q %v", data, err)
	}
}
