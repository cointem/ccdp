package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

func TestReviewUsesTaskPolicyAndAdmission(t *testing.T) {
	parent, workspace := codingFixture(t)
	parent.services.supervisor.directory.SetInteractive(true)
	external := t.TempDir()
	path := filepath.Join(external, "context.txt")
	if err := os.WriteFile(path, []byte("external context"), 0600); err != nil {
		t.Fatal(err)
	}
	var taskAllowed map[string]bool
	for _, purpose := range []string{childPurposeTask, childPurposeReview} {
		cfg, opts, lease, catalog, hooks, err := parent.services.supervisor.prepare(parent, purpose, "")
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		if purpose == childPurposeTask {
			taskAllowed = opts.AllowedTools
		} else {
			bindReviewInstructions(&cfg)
			if !reflect.DeepEqual(taskAllowed, opts.AllowedTools) {
				t.Fatal("review has a special tool allowlist")
			}
		}
		if cfg.Workspace != workspace || opts.NonInteractive || opts.ReadOnlyWorkspace {
			t.Fatalf("%s changed workspace or interaction policy", purpose)
		}
		child, err := (childFactory{config: cfg, options: opts, tools: catalog, preHooks: hooks}).openAgent(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		for _, name := range []string{"Read", "Bash", "Write", "WebFetch", "SendMessage"} {
			call := messages.ToolCall{Name: name}
			if _, reason, _, denied := child.hardAdmitTool(call, toolJournalContext{}, nil); denied {
				t.Fatalf("%s denied %s: %s", purpose, name, reason)
			}
		}
		call := messages.ToolCall{Name: "Read", Arguments: map[string]any{"file_path": path}}
		tctx := child.buildToolContext(call, context.Background(), *child.services.cfg, child.services.sandbox, child.services.resources, nil)
		if out, err := tools.NewReadTool().Run(tctx); err != nil || !strings.Contains(out, "external context") {
			t.Fatalf("%s cannot read ordinary external context: %q %v", purpose, out, err)
		}
		// Interactive reviews may request additional authority through the same
		// approval flow as tasks; this must not silently grant it.
		call = messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": path, "content": "changed", "mode": "replace"}}
		tool, _ := child.services.registry.Get("Write")
		_, requested, _, err := child.toolCapabilityPolicy(call, tool)
		if err != nil || len(requested) != 1 || requested[0].Kind != "directory" {
			t.Fatalf("%s capability approval: %v, %v", purpose, requested, err)
		}
		if _, err := child.services.sandbox.ResolveWrite(path); err == nil {
			t.Fatal("capability request granted itself")
		}
		// Task-wide restrictions continue to apply to reviews too.
		if denied, _ := ChildToolGate(child, messages.ToolCall{Name: "SpawnAgent"}, tools.NewReadTool()); !denied {
			t.Fatal("review bypassed the ordinary nested task restriction")
		}
		child.services.sandbox.AddDisallowedDir(external)
		tctx.Sandbox = child.services.sandbox.Snapshot()
		if _, err := tools.NewReadTool().Run(tctx); err == nil {
			t.Fatal("explicit read denial was bypassed")
		}
	}
}
