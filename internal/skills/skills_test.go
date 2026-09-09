package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, fm, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + fm + "---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

func TestLoadAndGet(t *testing.T) {
	user := t.TempDir()
	proj := t.TempDir()

	writeSkill(t, user, "review",
		"name: code-review\ndescription: Review a diff for bugs and style.\n",
		"# Code Review\nAlways check error handling.\n")
	writeSkill(t, proj, "go-build",
		"name: go-build\ndescription: Build and test Go code.\n",
		"# Go Build\nRun go build and go test.\n")

	s := NewStore()
	s.Load(user, proj)

	if got := len(s.All()); got != 2 {
		t.Fatalf("expected 2 skills, got %d", got)
	}
	sk, ok := s.Get("code-review")
	if !ok {
		t.Fatal("missing code-review skill")
	}
	if sk.Source != "user" || !strings.Contains(sk.Body, "error handling") {
		t.Errorf("bad code-review skill: %+v", sk)
	}
	gb, ok := s.Get("go-build")
	if !ok || gb.Source != "project" {
		t.Errorf("expected project skill go-build, got %+v ok=%v", gb, ok)
	}
	sec := s.SkillsSection()
	if !strings.Contains(sec, "code-review") || !strings.Contains(sec, "go-build") {
		t.Errorf("SkillsSection missing skills: %q", sec)
	}
}

func TestProjectOverridesUser(t *testing.T) {
	user := t.TempDir()
	proj := t.TempDir()
	writeSkill(t, user, "x", "name: dup\ndescription: user version\n", "user body")
	writeSkill(t, proj, "x", "name: dup\ndescription: project version\n", "project body")

	s := NewStore()
	s.Load(user, proj)
	sk, ok := s.Get("dup")
	if !ok {
		t.Fatal("missing dup")
	}
	if !strings.Contains(sk.Body, "project body") {
		t.Errorf("project skill should override user: %+v", sk)
	}
}

func TestFoldedDescription(t *testing.T) {
	user := t.TempDir()
	writeSkill(t, user, "multi",
		"name: multi\ndescription: line one\n  line two\n",
		"body")

	s := NewStore()
	s.Load(user)
	sk, _ := s.Get("multi")
	if !strings.Contains(sk.Description, "line two") {
		t.Errorf("folded description not parsed: %q", sk.Description)
	}
}

func TestNoFrontmatterFallsBack(t *testing.T) {
	user := t.TempDir()
	dir := filepath.Join(user, "plain")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# plain skill\nbody"), 0o644)

	s := NewStore()
	s.Load(user)
	sk, ok := s.Get("plain")
	if !ok {
		t.Fatal("expected fallback name from directory")
	}
	if sk.Description != "" {
		t.Errorf("expected empty description, got %q", sk.Description)
	}
}
