package permissions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyApprovalRequiresExecutableIdentity(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"cat", "head", "ls", "env", "echo", "true", "pwd"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root+":/usr/bin:/bin")
	for _, cmd := range []string{"cat victim", "head victim", "ls", "env echo hello", "env true", "env pwd", "env"} {
		m := NewManager(ModeDefault, Policy{})
		if d, why := m.Check("Bash", map[string]any{"command": cmd}, KindShell); d != DecisionAsk {
			t.Fatalf("%q: %s %s", cmd, d, why)
		}
	}
	// The shell resolves these builtins independently of PATH.
	for _, cmd := range []string{"echo hello", "true", "pwd"} {
		m := NewManager(ModeDefault, Policy{})
		if d, why := m.Check("Bash", map[string]any{"command": cmd}, KindShell); d != DecisionAllow {
			t.Fatalf("builtin %q: %s %s", cmd, d, why)
		}
	}
	// An env wrapper invokes even builtin names as external executables.
	if err := os.Remove(filepath.Join(root, "env")); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"env echo hello", "env true", "env pwd"} {
		m := NewManager(ModeDefault, Policy{})
		if d, why := m.Check("Bash", map[string]any{"command": cmd}, KindShell); d != DecisionAsk {
			t.Fatalf("external builtin %q: %s %s", cmd, d, why)
		}
	}
	m := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:cat victim"}})
	if d, why := m.Check("Bash", map[string]any{"command": "cat victim"}, KindShell); d != DecisionAllow {
		t.Fatalf("explicit approval lost: %s %s", d, why)
	}
}

func TestReadOnlyApprovalRejectsCwdDependentPATH(t *testing.T) {
	for _, path := range []string{"relative-tools:/usr/bin:/bin", ":/usr/bin:/bin"} {
		t.Setenv("PATH", path)
		m := NewManager(ModeDefault, Policy{})
		if decision, reason := m.Check("Bash", map[string]any{"command": "cat victim"}, KindShell); decision != DecisionAsk {
			t.Fatalf("PATH=%q: %s %s", path, decision, reason)
		}
	}
}
