package permissions

import "testing"

func TestUnknownToolNamesNeverGrantBuiltinCapabilities(t *testing.T) {
	for _, name := range []string{"Read", "Write", "Bash", "GitStatus", "Task", "GetGoal"} {
		if decision, _ := NewManager(ModeDefault, Policy{}).Check(name, nil, KindUnknown); decision != DecisionAsk {
			t.Fatalf("unknown implementation %s inherited a builtin permission: %s", name, decision)
		}
	}
}
