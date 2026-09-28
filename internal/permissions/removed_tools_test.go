package permissions

import "testing"

func TestRemovedToolNamesAreNotBuiltinCapabilities(t *testing.T) {
	for _, name := range []string{"GitStatus", "GitDiff", "GitLog", "GitCommit", "ProcessStart", "ProcessWrite", "ProcessOutput", "ProcessStop", "Task"} {
		effects := InvocationEffects(name, nil)
		if len(effects) != 1 || effects[0] != EffectUnknown {
			t.Fatalf("%s: %v", name, effects)
		}
	}
}
