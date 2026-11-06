package hooks

import (
	"ccdp/internal/testenv"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHookRunFreezesPolicyBeforePayloadMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "message")
	if err := os.WriteFile(path, []byte(`{"additionalContext":"original policy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	policy := testenv.Sandbox(t, dir)
	m := NewManager(Config{EventPreToolUse: []HookSpec{{Command: "cat message"}}}, Options{Workspace: dir, Sandbox: policy, FailClosed: true})
	out := m.Run(context.Background(), EventPreToolUse, func(*Input) { policy.AddDisallowedDir(path) })
	if out.AdditionalContext != "original policy" {
		t.Fatalf("hook observed changed policy: %+v", out)
	}
	out = m.Run(context.Background(), EventPreToolUse, nil)
	if out.Decision != DecisionDeny {
		t.Fatalf("subsequent hook did not observe deny: %+v", out)
	}
}
