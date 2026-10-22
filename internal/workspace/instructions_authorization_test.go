package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

func TestInstructionsAuthorizeUserAndProjectSources(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		t.Run(scope, func(t *testing.T) {
			root, home, private := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			source := filepath.Join(private, "secret.md")
			if err := os.WriteFile(source, []byte("DENIED INSTRUCTIONS"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, InstructionFile)
			if scope == "user" {
				path = filepath.Join(home, UserInstructionFile)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(source, path); err != nil {
				t.Fatal(err)
			}
			policy := sandbox.New(root)
			out, err := LoadInstructionsChecked(root, policy.ResolveRead)
			if err != nil || !strings.Contains(out, "DENIED INSTRUCTIONS") {
				t.Fatalf("authorized instruction alias: out=%q err=%v", out, err)
			}
			policy.AddDisallowedDir(private)
			out, err = LoadInstructionsChecked(root, policy.ResolveRead)
			if err == nil || strings.Contains(out, "DENIED INSTRUCTIONS") {
				t.Fatalf("denied instructions entered prompt: out=%q err=%v", out, err)
			}
		})
	}
}

func TestInstructionsAbsentDeniedCandidateRemainsOptional(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	policy := sandbox.New(root)
	policy.AddDisallowedDir(home)
	if out, err := LoadInstructionsChecked(root, policy.ResolveRead); err != nil || out != "" {
		t.Fatalf("absent user instructions: out=%q err=%v", out, err)
	}
	if _, err := LoadInstructionsChecked(root, nil); err == nil {
		t.Fatal("accepted missing read policy")
	}
}
