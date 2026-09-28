package permissions

import "testing"

func TestWriteCapableCommandsRequireApproval(t *testing.T) {
	m := NewManager(ModeDefault, Policy{})
	for _, command := range []string{
		"touch marker", "env touch marker", "git diff",
		"git diff --ext-diff", "git diff --textconv",
		"git diff --no-ext-diff --no-textconv --output=result",
		"git diff --no-ext-diff --no-textconv --output result",
	} {
		if decision, reason := m.Check("Bash", map[string]any{"command": command}); decision != DecisionAsk {
			t.Errorf("%s: %s (%s)", command, decision, reason)
		}
	}
	for _, command := range []string{"git diff --no-ext-diff --no-textconv", "git diff --no-ext-diff --no-textconv -- a.txt"} {
		if decision, reason := m.Check("Bash", map[string]any{"command": command}); decision != DecisionAllow {
			t.Errorf("%s: %s (%s)", command, decision, reason)
		}
	}
}
