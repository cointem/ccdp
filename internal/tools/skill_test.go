package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
)

func TestReadSkillReauthorizesCachedOriginalAndSource(t *testing.T) {
	for _, denySource := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-path", true: "retargeted-source"}[denySource], func(t *testing.T) {
			root := t.TempDir()
			skillRoot := filepath.Join(root, "skills")
			skillDir := filepath.Join(skillRoot, "linked")
			private := filepath.Join(root, "source")
			for _, dir := range []string{skillDir, private} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			source := filepath.Join(private, "instructions.md")
			if err := os.WriteFile(source, []byte("---\nname: linked\n---\nCACHED SECRET"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(skillDir, "SKILL.md")
			if err := os.Symlink(source, link); err != nil {
				t.Fatal(err)
			}
			policy := sandbox.New(root)
			store := skills.NewStore()
			if err := store.LoadChecked(policy.ResolveRead, "", skillRoot); err != nil {
				t.Fatal(err)
			}
			ctx := &Context{Context: context.Background(), WorkingDir: root, Sandbox: policy, Skills: store, Args: map[string]any{"name": "linked"}}
			if out, err := executeToolText(NewReadSkillTool(), ctx); err != nil || !strings.Contains(out, "CACHED SECRET") {
				t.Fatalf("authorized cached skill: out=%q err=%v", out, err)
			}
			if denySource {
				allowed := filepath.Join(root, "allowed.md")
				if err := os.WriteFile(allowed, []byte("allowed"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(allowed, link); err != nil {
					t.Fatal(err)
				}
				policy.AddDisallowedDir(private)
				if _, err := policy.ResolveRead(link); err != nil {
					t.Fatalf("retargeted original path should be allowed: %v", err)
				}
			} else {
				policy.AddDisallowedDir(skillRoot)
			}
			if out, err := executeToolText(NewReadSkillTool(), ctx); err == nil || out != "" {
				t.Fatalf("cached skill leaked after revocation: out=%q err=%v", out, err)
			}
		})
	}
}
