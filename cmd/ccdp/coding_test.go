package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCLIWaitsForChildReport(t *testing.T) {
	testReviewCLIReport(t, false)
}

func TestReviewCLIPartialProducesReportAndExitTwo(t *testing.T) {
	testReviewCLIReport(t, true)
}

func testReviewCLIReport(t *testing.T, partial bool) {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, output)
		}
	}
	if partial {
		if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte("\x00binary"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(map[string]any{"session_dir": t.TempDir(), "model": "test/model", "providers": map[string]any{"test": map[string]any{"base_url": "http://127.0.0.1:1", "wire_api": "chat", "models": map[string]any{"model": map[string]any{}}}}, "enable_guardian": false, "enable_memory": false})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	wantCode, wantText := 0, "所选范围无变更"
	if partial {
		wantCode, wantText = 2, "未覆盖"
	}
	if code := runCodingCLI([]string{"review", "-d", root, "--config-file", path, "--permission-mode", "bypass"}, &out, &diagnostics); code != wantCode {
		t.Fatalf("exit %d: %s; %s", code, diagnostics.String(), out.String())
	}
	if !strings.Contains(out.String(), wantText) || strings.Contains(out.String(), "review_session_id") {
		t.Fatalf("CLI did not wait for final report: %s", out.String())
	}
}

func TestCodingCLIUsesDurableOperation(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\n"), 0644)
	data, e := json.Marshal(map[string]any{"session_dir": t.TempDir(), "model": "test/model", "providers": map[string]any{"test": map[string]any{"base_url": "http://127.0.0.1:1", "wire_api": "chat", "models": map[string]any{"model": map[string]any{}}}}, "enable_guardian": false, "enable_memory": false})
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	var out, err bytes.Buffer
	code := runCodingCLI([]string{"index", "status", "-d", root, "--config-file", path}, &out, &err)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err.String())
	}
	if !strings.Contains(out.String(), "1 files") || !strings.Contains(err.String(), "session:") {
		t.Fatalf("output=%q diagnostics=%q", out.String(), err.String())
	}
}
