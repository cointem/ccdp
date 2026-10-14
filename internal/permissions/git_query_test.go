package permissions

import "testing"

func TestTypedGitQueryRetainsPolicy(t *testing.T) {
	argv := []string{"git", "status", "--short"}
	for _, mode := range []Mode{ModeDefault, ModePlan, ModeAcceptEdits, ModeBypass} {
		m := NewManager(mode, Policy{})
		if d, reason := m.CheckReadOnlyGit(argv); d != DecisionAllow {
			t.Fatalf("%s query: %s %s", mode, d, reason)
		}
		m.SetPolicy(Policy{AlwaysDeny: []string{"Bash:git status*"}})
		if d, _ := m.CheckReadOnlyGit(argv); d != DecisionDeny {
			t.Fatalf("%s ignored persistent deny", mode)
		}
	}
	for _, command := range []string{"git status --short", "'git' 'status' '--short'"} {
		m := NewManager(ModeBypass, Policy{})
		m.RememberDeny(CommandKey(command))
		if d, _ := m.CheckReadOnlyGit(argv); d != DecisionDeny {
			t.Fatalf("ignored remembered deny %q", command)
		}
	}
	m := NewManager(ModeDefault, Policy{})
	if d, _ := m.Check("Bash", map[string]any{"command": "git status --short", "read_only": true, "typed_git": true}, KindShell); d != DecisionAsk {
		t.Fatal("argument-map flags forged typed permission")
	}
	for _, argv := range [][]string{{"git", "log", "--output=out"}, {"git", "push"}, {"echo", "hello"}} {
		if d, _ := NewManager(ModeBypass, Policy{}).CheckReadOnlyGit(argv); d == DecisionAllow {
			t.Fatalf("invalid typed query allowed: %q", argv)
		}
	}
	var unavailable *Manager
	if d, _ := unavailable.CheckReadOnlyGit(argv); d != DecisionDeny {
		t.Fatal("nil policy allowed")
	}
}
