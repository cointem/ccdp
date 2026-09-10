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
