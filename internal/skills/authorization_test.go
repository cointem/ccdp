package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

func TestLoadCheckedRejectsDeniedSkillBeforeIndexing(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "symlink"}[linked], func(t *testing.T) {
			root := t.TempDir()
			user := filepath.Join(root, "user")
			private := filepath.Join(root, "private")
			skillDir := writeSkill(t, private, "hidden", "name: hidden\ndescription: SECRET DESCRIPTION\n", "SECRET BODY")
			project := private
			if linked {
				project = filepath.Join(root, "project")
				linkDir := filepath.Join(project, "linked")
				if err := os.MkdirAll(linkDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(skillDir, "SKILL.md"), filepath.Join(linkDir, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			policy := sandbox.New(root)
			policy.AddDisallowedDir(private)
			store := NewStore()
			if err := store.LoadChecked(policy.ResolveRead, user, project); err == nil || !strings.Contains(err.Error(), "denied") {
				t.Fatalf("load denied skill: %v", err)
			}
			if len(store.All()) != 0 || store.SkillsSection() != "" {
				t.Fatal("denied skill content entered the store or prompt index")
			}
		})
	}
}

func TestLoadCheckedRecordsOpenedSourceAndAllowsUserSkillsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	user := t.TempDir()
	sourceDir := writeSkill(t, user, "original", "name: linked\ndescription: description\n", "body")
	project := filepath.Join(root, ".ccdp", "skills")
	linkDir := filepath.Join(project, "alias")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "SKILL.md")
	if err := os.Symlink(source, filepath.Join(linkDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.LoadChecked(sandbox.New(root).ResolveRead, user, project); err != nil {
		t.Fatal(err)
	}
	sk, ok := store.Get("linked")
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || sk.Path != linkDir || sk.CanonicalFile != canonical || sk.Source != "project" {
		t.Fatalf("incorrect cached provenance: %+v, ok=%v; source=%q", sk, ok, canonical)
	}
}

func TestLoadCheckedRequiresReadPolicy(t *testing.T) {
	if err := NewStore().LoadChecked(nil, t.TempDir()); err == nil {
		t.Fatal("accepted missing read policy")
	}
}
