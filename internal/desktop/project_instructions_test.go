package desktop

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/workspace"
)

func TestProjectInstructionsCreateAndEdit(t *testing.T) {
	for _, name := range []string{"AGENTS.md", ".ccdp/AGENTS.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			doc := readProjectInstruction(dir, name)
			if err := doc.save(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("opening the editor created an instruction file: %v", err)
			}
			doc.text = "# 开发约定\n\n运行测试。\n"
			if err := doc.save(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			if restored := readProjectInstruction(dir, name); restored.readError != nil || restored.text != doc.text {
				t.Fatalf("saved instruction was not restored: %+v", restored)
			}

			doc.text += "检查格式。\n"
			if err := doc.save(t.Context(), dir); err != nil {
				t.Fatalf("second save of the same editor: %v", err)
			}
			const external = "同事从其他编辑器保存的约定。"
			if err := os.WriteFile(path, []byte(external), 0644); err != nil {
				t.Fatal(err)
			}
			doc.text = "旧草稿"
			if err := doc.save(t.Context(), dir); err == nil {
				t.Fatal("overwrote instructions from another editor")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != external {
				t.Fatalf("lost external edit: %q %v", got, err)
			}
		})
	}
}

func TestProjectInstructionsRejectOutsideLink(t *testing.T) {
	for _, name := range []string{"AGENTS.md", ".ccdp/AGENTS.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			external := t.TempDir()
			target := filepath.Join(external, "AGENTS.md")
			const outsideText = "仅属于另外一个项目"
			if err := os.WriteFile(target, []byte(outsideText), 0644); err != nil {
				t.Fatal(err)
			}
			// Exercise both the final file and its parent changing into a link
			// after the editor opens a missing instruction file.
			before := readProjectInstruction(dir, name)
			link, dest := filepath.Join(dir, name), target
			if name != "AGENTS.md" {
				link, dest = filepath.Join(dir, ".ccdp"), external
			}
			if err := os.Symlink(dest, link); err != nil {
				t.Fatal(err)
			}
			after := readProjectInstruction(dir, name)
			if after.readError == nil || after.text != "" {
				t.Fatal("the project editor followed a link outside its workspace")
			}
			before.text = "一个项目内的草稿"
			if err := before.save(t.Context(), dir); err == nil {
				t.Fatal("saved through an external link")
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != outsideText {
				t.Fatalf("changed external instructions: %q %v", got, err)
			}
		})
	}
}

func TestProjectInstructionsHonorRuntimeSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	doc := readProjectInstruction(dir, "AGENTS.md")
	tooLarge := strings.Repeat("x", workspace.DefaultMaxInstructionFileBytes+1)
	doc.text = tooLarge
	if err := doc.save(t.Context(), dir); err == nil {
		t.Fatal("accepted a draft exceeding the runtime limit")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oversized draft created a file: %v", err)
	}
	if err := os.WriteFile(path, []byte(tooLarge), 0644); err != nil {
		t.Fatal(err)
	}
	doc = readProjectInstruction(dir, "AGENTS.md")
	if doc.readError == nil || doc.text != "" {
		t.Fatal("exposed a partial draft for an oversized existing file")
	}
	doc.text = "缩短"
	if err := doc.save(t.Context(), dir); err == nil {
		t.Fatal("overwrote an instruction the editor could not load")
	}
}
