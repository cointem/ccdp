package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

func globContext(workspace string, policy *sandbox.Sandbox, args map[string]any) *Context {
	return &Context{
		Context:    context.Background(),
		WorkingDir: workspace,
		Sandbox:    policy,
		FileState:  NewFileState("search-sandbox-test"),
		Args:       args,
	}
}

func TestReadOutsideWorkspaceHonorsExplicitDeny(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "reference.txt")
	if err := os.WriteFile(file, []byte("external reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	if out, err := NewReadTool().Run(globContext(workspace, policy, map[string]any{"file_path": file})); err != nil || !strings.Contains(out, "external reference") {
		t.Fatalf("external Read: output=%q error=%v", out, err)
	}
	policy.AddDisallowedDir(outside)
	if out, err := NewReadTool().Run(globContext(workspace, policy, map[string]any{"file_path": file})); err == nil || strings.Contains(out, "external reference") {
		t.Fatalf("Read bypassed explicit deny: output=%q error=%v", out, err)
	}
}

func TestLSFiltersDeniedEntries(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "visible.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(workspace, "secret-dir")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	policy.AddDisallowedDir(denied)
	out, err := NewLSTool().Run(globContext(workspace, policy, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret-dir") || !strings.Contains(out, "visible.txt") || !strings.Contains(out, "(1 entries)") {
		t.Fatalf("LS exposed denied entry or omitted authorized entry: %q", out)
	}
	if _, err := NewLSTool().Run(globContext(workspace, policy, map[string]any{"path": denied})); err == nil {
		t.Fatal("LS accepted denied explicit directory")
	}
}

func TestGrepDefaultBaseRechecksReadPolicy(t *testing.T) {
	workspace := t.TempDir()
	policy := sandbox.New(workspace)
	policy.AddDisallowedDir(workspace)
	if _, err := NewGrepTool().Run(globContext(workspace, policy, map[string]any{"pattern": "needle"})); err == nil {
		t.Fatal("Grep searched a newly denied default working directory")
	}
}

func TestGlobAllowsAbsoluteAndParentPatternsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "visible.go")
	secret := filepath.Join(outside, "private.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy := sandbox.New(workspace)
	tool := NewGlobTool()
	for _, pattern := range []string{
		filepath.Join(outside, "*.txt"),
		filepath.Join("..", "outside", "*.txt"),
	} {
		out, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": pattern}))
		if err != nil || !strings.Contains(out, secret) {
			t.Errorf("Glob(%q) did not find readable external file: output=%q error=%v", pattern, out, err)
		}
	}

	// A .. component is still useful when normalization leaves the match inside
	// the workspace; authorization follows the resolved path rather than
	// banning the syntax itself.
	pattern := filepath.Join("..", filepath.Base(workspace), "*.go")
	out, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": pattern}))
	if err != nil {
		t.Fatalf("Glob(%q): %v", pattern, err)
	}
	if !strings.Contains(out, filepath.Base(inside)) {
		t.Fatalf("Glob(%q) missed authorized match: %s", pattern, out)
	}
}

func TestGlobAbsolutePatternWithinReadGrant(t *testing.T) {
	workspace := t.TempDir()
	readonly := t.TempDir()
	target := filepath.Join(readonly, "visible.txt")
	if err := os.WriteFile(target, []byte("visible"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	policy.AddReadOnlyDir(readonly)

	out, err := NewGlobTool().Run(globContext(workspace, policy, map[string]any{
		"pattern": filepath.Join(readonly, "*.txt"),
	}))
	if err != nil {
		t.Fatalf("absolute Glob in read grant: %v", err)
	}
	if !strings.Contains(out, target) {
		t.Fatalf("absolute Glob missed authorized path %q: %s", target, out)
	}
}

func TestGlobRecursivePrunesDisallowedDirectories(t *testing.T) {
	workspace := t.TempDir()
	allowed := filepath.Join(workspace, "src", "nested", "visible.txt")
	deniedDir := filepath.Join(workspace, "private")
	denied := filepath.Join(deniedDir, "secret.txt")
	if err := os.MkdirAll(filepath.Dir(allowed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowed, []byte("visible"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(denied, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	policy.AddDisallowedDir(deniedDir)

	out, err := NewGlobTool().Run(globContext(workspace, policy, map[string]any{"pattern": "**/*.txt"}))
	if err != nil {
		t.Fatalf("recursive Glob: %v", err)
	}
	if !strings.Contains(out, allowed) {
		t.Fatalf("recursive Glob missed allowed path %q: %s", allowed, out)
	}
	if strings.Contains(out, deniedDir) || strings.Contains(out, filepath.Base(denied)) {
		t.Fatalf("recursive Glob exposed a disallowed path: %s", out)
	}
}

func TestGlobFiltersDisallowedMatchesAndRejectsDisallowedAnchor(t *testing.T) {
	workspace := t.TempDir()
	deniedDir := filepath.Join(workspace, "private")
	allowed := filepath.Join(workspace, "visible.txt")
	denied := filepath.Join(deniedDir, "secret.txt")
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{allowed: "visible", denied: "secret"} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	policy := sandbox.New(workspace)
	policy.AddDisallowedDir(deniedDir)
	tool := NewGlobTool()

	out, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": "*.txt"}))
	if err != nil {
		t.Fatalf("Glob with a denied sibling: %v", err)
	}
	if !strings.Contains(out, allowed) || strings.Contains(out, filepath.Base(denied)) {
		t.Fatalf("Glob returned an invalid result set: %s", out)
	}

	out, err = tool.Run(globContext(workspace, policy, map[string]any{"pattern": "private/**/*.txt"}))
	if err == nil {
		t.Fatalf("Glob under an explicitly denied anchor unexpectedly succeeded: %s", out)
	}
	if strings.Contains(out, filepath.Base(denied)) || strings.Contains(err.Error(), filepath.Base(denied)) {
		t.Fatalf("Glob exposed a denied path: output=%q error=%q", out, err)
	}
}

func TestGlobPreservesSimpleAndRecursivePatterns(t *testing.T) {
	workspace := t.TempDir()
	rootFile := filepath.Join(workspace, "root.go")
	nestedFile := filepath.Join(workspace, "pkg", "nested.go")
	if err := os.MkdirAll(filepath.Dir(nestedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rootFile, nestedFile} {
		if err := os.WriteFile(path, []byte("package p"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewGlobTool()
	policy := sandbox.New(workspace)

	rootOut, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": "*.go"}))
	if err != nil {
		t.Fatalf("root Glob: %v", err)
	}
	if !strings.Contains(rootOut, rootFile) || strings.Contains(rootOut, nestedFile) {
		t.Fatalf("simple Glob should match only the root file: %s", rootOut)
	}

	recursiveOut, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": "**/*.go"}))
	if err != nil {
		t.Fatalf("recursive Glob: %v", err)
	}
	if !strings.Contains(recursiveOut, rootFile) || !strings.Contains(recursiveOut, nestedFile) {
		t.Fatalf("recursive Glob missed expected paths: %s", recursiveOut)
	}

	dotOut, err := tool.Run(globContext(workspace, policy, map[string]any{"pattern": "."}))
	if err != nil {
		t.Fatalf("literal directory Glob: %v", err)
	}
	if !strings.Contains(dotOut, workspace) {
		t.Fatalf("literal directory Glob missed its authorized path: %s", dotOut)
	}
}
