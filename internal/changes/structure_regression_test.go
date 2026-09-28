package changes

import (
	"ccdp/internal/sandbox"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStructureApplyRecoversAtEveryStep(t *testing.T) {
	for _, toDir := range []bool{false, true} {
		for stop := 0; stop < 5; stop++ {
			t.Run(fmt.Sprintf("toDir=%v/step=%d", toDir, stop), func(t *testing.T) {
				root := t.TempDir()
				root, _ = filepath.EvalSymlinks(root)
				s := New(t.TempDir())
				file, err := s.ImportFile([]byte("content"), 0600, "regular")
				if err != nil {
					t.Fatal(err)
				}
				base := Snapshot{Root: root, Files: map[string]File{"a": file}}
				target := Snapshot{Root: root, Files: map[string]File{"a/b/c": file}}
				if !toDir {
					base, target = target, base
				}
				if err = s.Materialize(context.Background(), base, root); err != nil {
					t.Fatal(err)
				}
				plan, err := s.Prepare(base, target, base, nil)
				if err != nil {
					t.Fatal(err)
				}
				steps, _ := applySteps(plan)
				if stop >= len(steps) {
					return
				}
				handle, err := os.OpenRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				// Simulate process loss after the filesystem step, before journal advance.
				for _, step := range steps[:stop+1] {
					if step.after.Kind == "directory" {
						err = handle.Mkdir(step.path, 0755)
						if os.IsExist(err) {
							err = nil
						}
					} else {
						var data []byte
						if step.after.Kind != "absent" {
							data, err = s.Content(step.after)
						}
						if err == nil {
							err = writeRooted(context.Background(), handle, step.path, step.after, data)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if err = s.save("transactions", plan.ID, Journal{Plan: plan, Status: "applying", NextStep: stop}); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Apply(context.Background(), plan, sandbox.New(root)); err != nil {
					t.Fatal(err)
				}
				for path := range target.Files {
					got, err := os.ReadFile(filepath.Join(root, path))
					if err != nil || string(got) != "content" {
						t.Fatalf("%s: %q %v", path, got, err)
					}
				}
			})
		}
	}
}

func TestStructureApplyRejectsUnselectedFilesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	s := New("")
	file, _ := s.ImportFile([]byte("keep"), 0644, "regular")
	base := Snapshot{Root: root, Files: map[string]File{"a/b": file}}
	target := Snapshot{Root: root, Files: map[string]File{"a": file}}
	if err := s.Materialize(context.Background(), base, root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a/extra"), []byte("extra"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Prepare(base, target, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(context.Background(), plan, sandbox.New(root)); err == nil {
		t.Fatal("unselected file removed")
	}
	for _, path := range []string{"a/b", "a/extra"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("preflight mutated %s: %v", path, err)
		}
	}
}

func TestRegressionFileDirectoryTransition(t *testing.T) {
	for _, toDir := range []bool{true, false} {
		t.Run(map[bool]string{true: "file_to_dir", false: "dir_to_file"}[toDir], func(t *testing.T) {
			root := t.TempDir()
			root, _ = filepath.EvalSymlinks(root)
			s := New("")
			f, err := s.ImportFile([]byte("content"), 0644, "regular")
			if err != nil {
				t.Fatal(err)
			}
			base := Snapshot{Root: root, Files: map[string]File{"a": f}}
			target := Snapshot{Root: root, Files: map[string]File{"a/b": f}}
			if !toDir {
				base, target = target, base
			}
			if err := s.Materialize(context.Background(), base, root); err != nil {
				t.Fatal(err)
			}
			plan, err := s.Prepare(base, target, base, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Apply(context.Background(), plan, sandbox.New(root)); err != nil {
				t.Fatalf("valid replacement failed: %v", err)
			}
			for p := range target.Files {
				if _, err := os.Stat(filepath.Join(root, p)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
