package permissions

import (
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
