package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelevantRepoMapAuthorizesSelectedPaths(t *testing.T) {
	info := &Info{}
	for i := 0; i < 5000; i++ {
		info.Files = append(info.Files, Entry{RelPath: fmt.Sprintf("source%04d.go", i)})
	}
	checks := 0
	text := info.RelevantRepoMap(120, "source4999", func(path string) bool {
		checks++
		return path != "source0000.go"
	})
	if checks != 121 || strings.Contains(text, "source0000.go") || !strings.Contains(text, "source4999.go") {
		t.Fatalf("relevance/access filtering: checks=%d map=%s", checks, text)
	}
}

func TestBoundedScanLoadsOnlyApplicableGitignores(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		".git/HEAD": "ref: refs/heads/test", ".gitignore": "*.log\n",
		"src/.gitignore": "!keep.log\n*.tmp\n", "src/keep.log": "keep",
		"src/hide.log": "hide", "src/hide.tmp": "hide", "src/main.go": "package src",
		"unvisited/.gitignore": "*.go\n", "unvisited/hide.go": "hide",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{root, filepath.Join(root, "src")} {
		info, err := Scan(dir, 100)
		if err != nil {
			t.Fatal(err)
		}
		text := info.RepoMap(100)
		if !strings.Contains(text, "keep.log") || !strings.Contains(text, "main.go") || strings.Contains(text, "hide.") {
			t.Fatalf("nested/ancestor ignore precedence lost: %s", text)
		}
	}
	info, err := Scan(root, 2)
	if err != nil || !info.Truncated || len(info.Files) != 2 {
		t.Fatalf("unbounded inventory: %+v %v", info, err)
	}
}
