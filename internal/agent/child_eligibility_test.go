package agent

import (
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/tools"
)

func TestChildToolEligibilityPreservesArgumentScopedTools(t *testing.T) {
	child := &Agent{children: childRuntimeBindings{childState: &childRuntimeState{
		purpose:        childPurposeTask,
		nonInteractive: true,
		allowed:        map[string]bool{"Write": true},
		perms: permissions.NewManager(permissions.ModeManual, permissions.Policy{
			AlwaysAllow: []string{"Write:result.txt"},
		}),
	}}}
	write := tools.NewWriteTool()
	if denied, reason := childToolEligibility(child, "Write", write); denied {
		t.Fatalf("argument-scoped tool excluded from catalog: %s", reason)
	}
	for _, tc := range []struct {
		path   string
		denied bool
	}{{"result.txt", false}, {"other.txt", true}} {
		call := messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": tc.path, "content": "x"}}
		denied, reason := ChildToolGate(child, call, write)
		if denied != tc.denied {
			t.Fatalf("Write %q: denied=%v, reason=%q", tc.path, denied, reason)
		}
		if denied && !strings.Contains(reason, "non-interactive") {
			t.Fatalf("unexpected denial: %q", reason)
		}
	}
	if denied, _ := childToolEligibility(child, "Read", tools.NewReadTool()); !denied {
		t.Fatal("tool outside frozen allowlist entered catalog")
	}
}
