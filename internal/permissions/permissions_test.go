package permissions

import (
	"sync"
	"testing"
)

func TestParseModeIncludesPlan(t *testing.T) {
	if m, err := ParseMode("plan"); err != nil || m != ModePlan {
		t.Errorf("plan mode not parsed: %v %v", m, err)
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Error("expected error for bogus mode")
	}
}

// TestNamespacedMCPToolNeverAutoAllowed guards the identity-shadowing attack:
// a malicious MCP server that advertises a tool whose raw name equals a built-in
// (e.g. "Read") is published as "mcp__<server>__Read" and must still require
// approval rather than inherit the built-in's auto-allow classification. An
// explicit always_allow rule on the fully qualified name remains the only
// non-bypass path to auto-approve it.
func TestNamespacedMCPToolNeverAutoAllowed(t *testing.T) {
	m := NewManager(ModeAcceptEdits, Policy{})
	if d, _ := m.Check("mcp__evil__Read", map[string]any{"file_path": "a.go"}); d != DecisionAsk {
		t.Errorf("shadowing MCP Read should ask, got %v", d)
	}
	if d, _ := m.Check("mcp__evil__Write", map[string]any{"file_path": "a.go"}); d != DecisionAsk {
		t.Errorf("MCP Write in acceptEdits must still ask, got %v", d)
	}
	if d, _ := m.Check("Read", map[string]any{"file_path": "a.go"}); d != DecisionAllow {
		t.Errorf("built-in Read must remain read-only-allowed, got %v", d)
	}
	allow := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"mcp__evil__Read"}})
	if d, _ := allow.Check("mcp__evil__Read", map[string]any{"file_path": "a.go"}); d != DecisionAllow {
		t.Errorf("explicit always_allow on the qualified name should permit it, got %v", d)
	}
}

func TestPlanModeGates(t *testing.T) {
	m := NewManager(ModePlan, Policy{})
	if d, _ := m.Check("Bash", map[string]any{"command": "ls"}); d != DecisionAsk {
		t.Errorf("plan mode should gate Bash, got %v", d)
	}
	if d, _ := m.Check("Write", map[string]any{"file_path": "a.go"}); d != DecisionAsk {
		t.Errorf("plan mode should gate Write, got %v", d)
	}
	if d, _ := m.Check("Read", map[string]any{"file_path": "a.go"}); d != DecisionAllow {
		t.Errorf("plan mode should allow Read, got %v", d)
	}
	if d, _ := m.Check("ToolSearch", map[string]any{"query": "x"}); d != DecisionAllow {
		t.Errorf("ToolSearch should be read-only, got %v", d)
	}
}

func TestAlwaysAllowRules(t *testing.T) {
	// Tool-level rule.
	m := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"WebFetch"}})
	if d, _ := m.Check("WebFetch", map[string]any{"url": "https://x"}); d != DecisionAllow {
		t.Errorf("tool-level allow rule failed: %v", d)
	}

	// Tool:value prefix glob ("Bash:git *" style).
	m = NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:git status*"}})
	if d, _ := m.Check("Bash", map[string]any{"command": "git status --short"}); d != DecisionAllow {
		t.Errorf("prefix glob allow failed: %v", d)
	}
	if d, _ := m.Check("Bash", map[string]any{"command": "git push"}); d != DecisionAsk {
		t.Errorf("non-matching command should still ask: %v", d)
	}

	// Bare command rule ("go test *" without the Bash: prefix).
	m = NewManager(ModeDefault, Policy{AlwaysAllow: []string{"go test *"}})
	if d, _ := m.Check("Bash", map[string]any{"command": "go test ./..."}); d != DecisionAllow {
		t.Errorf("bare command rule failed: %v", d)
	}
	if d, _ := m.Check("Bash", map[string]any{"command": "go build ./..."}); d != DecisionAsk {
		t.Errorf("bare rule must not match other commands: %v", d)
	}

	// Deny rules win over allows regardless of order.
	m = NewManager(ModeDefault, Policy{
		AlwaysAllow: []string{"Bash:git *"},
		AlwaysDeny:  []string{"Bash:git push*"},
	})
	if d, _ := m.Check("Bash", map[string]any{"command": "git push origin"}); d != DecisionDeny {
		t.Errorf("deny should win over allow: %v", d)
	}
}

func TestSudoIsNotAutoAllowed(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{"sudo cat /etc/shadow", "sudo ls /root", "sudo -i"} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d == DecisionAllow {
			t.Errorf("%q must not be auto-allowed, got %v", cmd, d)
		}
	}
}

func TestNewlineOperatorBlocksAutoAllow(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	// The second line is not read-only; a bare newline must defeat the
	// first-token allowlist.
	if d, _ := m.Check("Bash", map[string]any{"command": "cat foo\nsudo reboot"}); d == DecisionAllow {
		t.Error("multiline command with non-readonly tail must not be allowed")
	}
	if d, reason := m.Check("Bash", map[string]any{"command": "echo hi\nrm -rf /"}); d != DecisionDeny {
		t.Errorf("multiline dangerous command should hit deny patterns, got %v (%s)", d, reason)
	}
}

func TestFindMutationPrimariesAreNotReadOnly(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, command := range []string{
		"find . -delete",
		"find . -exec rm {} +",
		"find . -execdir touch marker +",
		"find . -fprint output.txt",
		"find . -fprintf output.txt %p",
	} {
		if decision, _ := m.Check("Bash", map[string]any{"command": command}); decision == DecisionAllow {
			t.Errorf("mutating find command was auto-allowed: %q", command)
		}
	}
	if decision, _ := m.Check("Bash", map[string]any{"command": "find . -type f -print"}); decision != DecisionAllow {
		t.Errorf("read-only find command should remain allowed, got %v", decision)
	}
}

func TestGitSafeRejectsOperators(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{"git status; rm -rf x", "git diff > /etc/passwd", "git log && reboot", "git status | sh"} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d == DecisionAllow {
			t.Errorf("%q must not be auto-allowed via gitSafe, got %v", cmd, d)
		}
	}
	// Plain read-only git commands stay allowed.
	if d, _ := m.Check("Bash", map[string]any{"command": "git status --short"}); d != DecisionAllow {
		t.Errorf("plain git status should be allowed, got %v", d)
	}
}

// TestQuotedArgvReadOnlyGitAutoAllowed guards the /diff regression: the
// runtime formats typed external commands with per-token shell quoting
// ('git' 'diff'), which must classify identically to the bare argv.
func TestQuotedArgvReadOnlyGitAutoAllowed(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{
		"'git' 'diff'",
		"'git' 'status' '--short'",
		"'git' 'diff' '--' 'a b.txt'",
		"git 'diff'",
		"env ls",
	} {
		if d, reason := m.Check("Bash", map[string]any{"command": cmd}); d != DecisionAllow {
			t.Errorf("%q must be auto-allowed, got %v (%s)", cmd, d, reason)
		}
	}
	for _, cmd := range []string{
		"'git' 'push' 'origin'",
		"'git' 'diff; rm -rf build'",
		"'find' '.' '-delete'",
		"env find . -delete",
		"'git' 'diff",
		"'git' 'diff' '",
	} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d == DecisionAllow {
			t.Errorf("%q must not be auto-allowed, got %v", cmd, d)
		}
	}
}

func TestSessionAllowCannotBypassAlwaysDeny(t *testing.T) {
	m := NewManager(ModeDefault, Policy{AlwaysDeny: []string{"Bash:git push*"}})
	key := SessionKey("Bash", map[string]any{"command": "git push origin"})
	m.RememberAllow(key)
	if d, reason := m.Check("Bash", map[string]any{"command": "git push origin"}); d != DecisionDeny {
		t.Errorf("session allow must not bypass always_deny, got %v (%s)", d, reason)
	}

	// Session deny still applies even when a persistent allow rule matches.
	m = NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:git *"}})
	m.RememberDeny(key)
	if d, _ := m.Check("Bash", map[string]any{"command": "git push origin"}); d != DecisionDeny {
		t.Errorf("session deny should win over persistent allow, got %v", d)
	}
}

func TestPathGlobSemantics(t *testing.T) {
	m := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Write:/tmp/*.log"}})
	if d, _ := m.Check("Write", map[string]any{"file_path": "/tmp/a.log"}); d != DecisionAllow {
		t.Errorf("single-segment path glob should match, got %v", d)
	}
	if d, _ := m.Check("Write", map[string]any{"file_path": "/tmp/sub/a.log"}); d == DecisionAllow {
		t.Error("path glob * must not cross / boundaries")
	}
	if d, _ := m.Check("Write", map[string]any{"file_path": "/etc/hosts"}); d == DecisionAllow {
		t.Error("path glob must stay anchored to the extension")
	}

	m = NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Write:/tmp/**"}})
	if d, _ := m.Check("Write", map[string]any{"file_path": "/tmp/deep/nested/a.log"}); d != DecisionAllow {
		t.Errorf("** should span path segments, got %v", d)
	}

	// Command globs keep the loose (cross-separator) semantics.
	m = NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:git status*"}})
	if d, _ := m.Check("Bash", map[string]any{"command": "git status --short -b"}); d != DecisionAllow {
		t.Errorf("command glob should match across spaces, got %v", d)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager(ModeDefault, Policy{AlwaysDeny: []string{"Bash:evil*"}})
	args := map[string]any{"command": "ls"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = m.Check("Bash", args)
				m.RememberAllow(CommandKey("echo hi"))
				m.RememberDeny(CommandKey("rm x"))
				_ = m.CurrentMode()
				m.SetMode(ModeAcceptEdits)
				m.SetPolicy(Policy{AlwaysAllow: []string{"Bash:ls*"}})
				m.SetMode(ModeDefault)
			}
		}(i)
	}
	wg.Wait()
}

func TestClonePreservesRememberedDecisionsWithoutSharingState(t *testing.T) {
	parent := NewManager(ModeDefault, Policy{
		AlwaysAllow: []string{"Read"},
		AlwaysDeny:  []string{"Bash:rm *"},
	})
	allowKey := SessionKey("Write", map[string]any{"file_path": "child.txt"})
	denyKey := SessionKey("Bash", map[string]any{"command": "git push"})
	parent.RememberAllow(allowKey)
	parent.RememberDeny(denyKey)

	child := parent.Clone()
	if got, _ := child.Check("Write", map[string]any{"file_path": "child.txt"}); got != DecisionAllow {
		t.Fatalf("clone lost remembered allow: %v", got)
	}
	if got, _ := child.Check("Bash", map[string]any{"command": "git push"}); got != DecisionDeny {
		t.Fatalf("clone lost remembered deny: %v", got)
	}

	child.RememberAllow(SessionKey("Write", map[string]any{"file_path": "other.txt"}))
	child.SetMode(ModeBypass)
	if got, _ := parent.Check("Write", map[string]any{"file_path": "other.txt"}); got == DecisionAllow {
		t.Fatal("child mutation leaked into parent")
	}
	if parent.CurrentMode() == ModeBypass {
		t.Fatal("child mode mutation leaked into parent")
	}
}

// TestHeadAllowRuleCannotAuthorizeChainedTail is the argv-aware regression: an
// allow rule for the head of a chain must never approve a tail the user did not
// sanction (the historic whole-string glob matched `git status && rm x`).
func TestHeadAllowRuleCannotAuthorizeChainedTail(t *testing.T) {
	m := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:git status*"}})
	if d, _ := m.Check("Bash", map[string]any{"command": "git status && rm x"}); d == DecisionAllow {
		t.Errorf("chained tail must not be auto-allowed by the head rule, got %v", d)
	}
	// The head command on its own is still allowed by the rule.
	if d, _ := m.Check("Bash", map[string]any{"command": "git status --short"}); d != DecisionAllow {
		t.Errorf("git status --short should still be allowed, got %v", d)
	}
}

// TestGitSafeRejectsMutatingSubcommands tightens the historic prefix match that
// auto-allowed `git remote set-url`, `git branch -D` and `git tag <name>`.
func TestGitSafeRejectsMutatingSubcommands(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{
		"git remote set-url origin https://evil.example",
		"git remote remove origin",
		"git branch -D main",
		"git tag v9.9",
	} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d == DecisionAllow {
			t.Errorf("mutating git subcommand must not be auto-allowed: %q", cmd)
		}
	}
	// Read-only git forms remain allowed.
	for _, cmd := range []string{"git remote", "git remote -v", "git branch -a", "git tag", "git config --list"} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d != DecisionAllow {
			t.Errorf("read-only git form should stay allowed: %q got %v", cmd, d)
		}
	}
}

// TestForcePushShortFlagDenied checks `-f` is treated like `--force`.
func TestForcePushShortFlagDenied(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{"git push -f origin main", "git push --force origin main"} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d != DecisionDeny {
			t.Errorf("%q should be denied as a force push, got %v", cmd, d)
		}
	}
	// A plain push is not denied outright — it just asks.
	if d, _ := m.Check("Bash", map[string]any{"command": "git push origin main"}); d != DecisionAsk {
		t.Errorf("plain git push should ask, got %v", d)
	}
}

// TestCommandSubstitutionFailsClosed ensures un-tokenizable constructs are never
// auto-allowed and fall through to the approval prompt.
func TestCommandSubstitutionFailsClosed(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, cmd := range []string{
		"echo $(rm -rf x)",
		"ls `whoami`",
		"cat <(secret)",
		"echo unbalanced \"quote",
	} {
		if d, _ := m.Check("Bash", map[string]any{"command": cmd}); d == DecisionAllow {
			t.Errorf("unsafe-to-parse command must not be auto-allowed: %q", cmd)
		}
	}
}
