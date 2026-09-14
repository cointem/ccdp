package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/messages"
)

func TestPersistedToolOutputCannotEscapeOutputDir(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.cfg.MaxResultSizeChars = 1024
	out := ag.maybePersistResult(messages.ToolCall{ID: "../../escaped"}, strings.Repeat("x", 2048))
	if _, err := os.Stat(filepath.Join(ag.cfg.SessionDir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("tool id escaped output directory: %v", err)
	}
	if !strings.Contains(out, "Full output retained in session artifact") {
		t.Fatalf("result does not reference the scoped artifact: %s", out)
	}
}

func TestLoadSessionRejectsTraversalID(t *testing.T) {
	if _, err := LoadSession(t.TempDir(), "../outside"); err == nil {
		t.Fatal("expected traversal session id to be rejected")
	}
}

func TestChildNoSessionPersistenceKeepsLargeOutputInMemory(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.cfg.MaxResultSizeChars = 1024
	ag.cfg.NoSessionPersistence = true
	ag.childState = &childRuntimeState{purpose: childPurposeTask}

	out := ag.maybePersistResult(messages.ToolCall{ID: "same-call-id"}, strings.Repeat("x", 4096))
	if len(out) > ag.cfg.MaxResultSizeChars+128 {
		t.Fatalf("child preview is not bounded: %d bytes", len(out))
	}
	if strings.Contains(out, "Full output:") {
		t.Fatalf("no-persistence child leaked a disk artifact: %s", out)
	}
	if _, err := os.Stat(filepath.Join(ag.cfg.SessionDir, "outputs")); !os.IsNotExist(err) {
		t.Fatalf("no-persistence child created parent output directory: %v", err)
	}
}

func TestChildLargeOutputUsesSessionOwnedDirectory(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.cfg.MaxResultSizeChars = 1024
	ag.childState = &childRuntimeState{purpose: childPurposeTask}

	out := ag.maybePersistResult(messages.ToolCall{ID: "same-call-id"}, strings.Repeat("x", 4096))
	if !strings.Contains(out, "Full output retained in session artifact") {
		t.Fatalf("child output does not reference its session artifact: %s", out)
	}
	if _, err := os.Stat(filepath.Join(ag.cfg.SessionDir, "outputs")); !os.IsNotExist(err) {
		t.Fatalf("child output created a shared outputs directory: %v", err)
	}
}
