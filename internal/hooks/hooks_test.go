package hooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPreToolUseDecision(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		EventPreToolUse: []HookSpec{
			{Command: `cat > /dev/null << 'EOF'
EOF
echo '{"decision":"deny","reason":"policy"}'
`},
		},
	}
	m := NewManager(cfg, Options{SessionID: "s1", Workspace: dir, Mode: "default"})
	out := m.PreToolUse(context.Background(), "Bash", map[string]any{"command": "rm x"})
	if out.Decision != DecisionDeny || out.Reason != "policy" {
		t.Errorf("PreToolUse = %+v, want deny/policy", out)
	}
}

func TestPreToolUseAllow(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{{Command: `echo '{"decision":"allow"}'`}},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.PreToolUse(context.Background(), "Write", map[string]any{})
	if out.Decision != DecisionAllow {
		t.Errorf("PreToolUse = %+v, want allow", out)
	}
}

func TestPostToolUseAppendsOutput(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPostToolUse: []HookSpec{{Command: `printf '%s' '{"hookSpecificOutput":"[checked]"}'`}},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.PostToolUse(context.Background(), "Bash", map[string]any{}, "out")
	if out.HookSpecificOutput == "" {
		t.Errorf("expected hookSpecificOutput, got %+v", out)
	}
}

func TestUserPromptSubmitBlock(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventUserPromptSubmit: []HookSpec{
			{Command: `input=$(cat); case "$input" in *ban*) echo '{"decision":"block","reason":"forbidden topic"}';; esac`},
		},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.UserPromptSubmit(context.Background(), "please ban this")
	if out.Decision != DecisionBlock {
		t.Errorf("UserPromptSubmit = %+v, want block", out)
	}
	out = m.UserPromptSubmit(context.Background(), "normal request")
	if out.Decision != DecisionNone {
		t.Errorf("UserPromptSubmit = %+v, want no decision", out)
	}
}

func TestNoHooks(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(nil, Options{Workspace: dir, Mode: "default"})
	if m.Has(EventPreToolUse) {
		t.Error("Has should be false with no config")
	}
	if out := m.PreToolUse(context.Background(), "Bash", nil); out.Decision != DecisionNone {
		t.Error("no hooks should produce no decision")
	}
}

func TestHookTimeoutNonBlocking(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{{Command: `sleep 5`}},
	}, Options{Workspace: dir, Mode: "default", Timeout: 200 * time.Millisecond})
	start := time.Now()
	out := m.PreToolUse(context.Background(), "Bash", nil)
	if time.Since(start) > 2*time.Second {
		t.Error("hook should have timed out quickly")
	}
	// A timed-out hook is a non-blocking error: the chain continues and the
	// tool call proceeds (Claude Code semantics — only exit 2 blocks).
	if out.Decision != DecisionNone {
		t.Errorf("timed-out hook should not veto, got %+v", out)
	}
	if out.Reason == "" {
		t.Error("timeout should be recorded in the reason for diagnostics")
	}
}

func TestExitCode2Blocks(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{{Command: `echo "no way" >&2; exit 2`}},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.PreToolUse(context.Background(), "Bash", map[string]any{"command": "x"})
	if out.Decision != DecisionBlock || out.Reason != "no way" {
		t.Errorf("exit 2 should block with stderr reason, got %+v", out)
	}
}

func TestExitOneNonBlocking(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{{Command: `echo "boom" >&2; exit 1`}},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.PreToolUse(context.Background(), "Bash", map[string]any{"command": "x"})
	if out.Decision != DecisionNone {
		t.Errorf("exit 1 is a non-blocking error, got %+v", out)
	}
	if out.Reason == "" {
		t.Error("failure should be recorded for diagnostics")
	}
}

func TestMatcherFiltersByToolName(t *testing.T) {
	dir := t.TempDir()
	ran := 0
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{
			{Matcher: "Bash", Command: `echo '{"decision":"deny","reason":"bash-hook"}'`},
			{Matcher: "Write|Edit", Command: `echo '{"decision":"deny","reason":"write-hook"}'`},
			{Matcher: `^Git`, Command: `echo '{"decision":"deny","reason":"git-hook"}'`},
			{Command: `echo '{"decision":"deny","reason":"all-hook"}'`},
		},
	}, Options{Workspace: dir, Mode: "default"})
	_ = ran

	// Bash: only the Bash matcher and the catch-all run; first veto wins.
	out := m.PreToolUse(context.Background(), "Bash", map[string]any{"command": "x"})
	if out.Reason != "bash-hook" {
		t.Errorf("matcher should select the Bash hook first, got %+v", out)
	}

	// Write: write-hook (regex alternative) then catch-all.
	out = m.PreToolUse(context.Background(), "Write", map[string]any{"file_path": "a"})
	if out.Reason != "write-hook" {
		t.Errorf("alternatives matcher should match Write, got %+v", out)
	}

	// GitLog: regex anchor matches, catch-all second.
	out = m.PreToolUse(context.Background(), "GitLog", map[string]any{})
	if out.Reason != "git-hook" {
		t.Errorf("regex matcher should match GitLog, got %+v", out)
	}

	// Read: only the catch-all matches.
	out = m.PreToolUse(context.Background(), "Read", map[string]any{"file_path": "a"})
	if out.Reason != "all-hook" {
		t.Errorf("catch-all should run for Read, got %+v", out)
	}
}

func TestAdditionalContextAggregated(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventUserPromptSubmit: []HookSpec{
			{Command: `echo '{"additionalContext":"ctx-1"}'`},
			{Command: `echo '{"additionalContext":"ctx-2"}'`},
		},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.UserPromptSubmit(context.Background(), "hi")
	if out.AdditionalContext != "ctx-1ctx-2" {
		t.Errorf("additionalContext should aggregate, got %q", out.AdditionalContext)
	}
}

func TestConfigUnmarshalBothShapes(t *testing.T) {
	var simple Config
	if err := unmarshal(t, `{"PreToolUse":["echo hi"]}`, &simple); err != nil {
		t.Fatal(err)
	}
	if len(simple["PreToolUse"]) != 1 || simple["PreToolUse"][0].Command != "echo hi" {
		t.Errorf("simple form not parsed: %+v", simple)
	}

	var structured Config
	if err := unmarshal(t, `{"PreToolUse":[{"matcher":"Bash","hooks":[{"command":"echo yo","timeout":30}]}]}`, &structured); err != nil {
		t.Fatal(err)
	}
	spec := structured["PreToolUse"][0]
	if spec.Matcher != "Bash" || spec.Command != "echo yo" || spec.Timeout != 30 {
		t.Errorf("structured form not parsed: %+v", spec)
	}
}

func unmarshal(t *testing.T, data string, v any) error {
	t.Helper()
	return json.Unmarshal([]byte(data), v)
}

func TestProjectDirEnv(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Config{
		EventPreToolUse: []HookSpec{{Command: `printf '%s' "{\"additionalContext\":\"$CCDP_PROJECT_DIR\"}"`}},
	}, Options{Workspace: dir, Mode: "default"})
	out := m.PreToolUse(context.Background(), "Bash", nil)
	if out.AdditionalContext != dir {
		t.Errorf("CCDP_PROJECT_DIR = %q, want %q", out.AdditionalContext, dir)
	}
}
