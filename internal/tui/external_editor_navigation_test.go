package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalEditorReturnsDraftToOriginalSession(t *testing.T) {
	m, _ := routingTestModel()
	m = switchRoutingTest(t, m, "child")
	m.textarea.SetValue("child unsent draft")
	m = switchRoutingTest(t, m, "root")
	m.textarea.SetValue("root draft")
	navigate := m.openAgentView("child")

	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("VISUAL", "/usr/bin/true")
	if m.openExternalEditor() == nil {
		t.Fatal("editor unavailable")
	}
	files, err := filepath.Glob(filepath.Join(dir, "ccdp-draft-*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("staged files=%v error=%v", files, err)
	}
	if err := os.WriteFile(files[0], []byte("edited root draft"), 0600); err != nil {
		t.Fatal(err)
	}
	msg := editorFinishedMsg{path: files[0], sessionID: m.sessionID, generation: m.attachmentGeneration}
	next, _ := m.Update(navigate())
	m = modelValue(t, next)
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	m = modelValue(t, next)
	next, _ = m.Update(msg)
	m = modelValue(t, next)
	if m.textarea.Value() != "child unsent draft" {
		t.Fatal("root edit replaced the child draft")
	}
	m = switchRoutingTest(t, m, "root")
	defer m.Close()
	if m.textarea.Value() != "edited root draft" {
		t.Fatal("edited root draft was lost")
	}
	if _, err := os.Stat(msg.path); !os.IsNotExist(err) {
		t.Fatal("editor staging file was not removed")
	}
}

func TestExternalEditorOldAttachmentCannotOverwriteDraft(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	path := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(path, []byte("old attachment draft"), 0600); err != nil {
		t.Fatal(err)
	}
	msg := editorFinishedMsg{path: path, sessionID: m.sessionID, generation: m.attachmentGeneration}
	m.attachmentGeneration++
	m.textarea.SetValue("new attachment draft")
	m.closeExternalEditor(msg)
	if m.textarea.Value() != "new attachment draft" {
		t.Fatal("stale editor return replaced the current attachment draft")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale editor staging file was not removed")
	}
}

func TestExternalEditorSurvivesWorkspaceState(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	m.textarea.SetValue("original draft")
	path := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(path, []byte("edited draft"), 0600); err != nil {
		t.Fatal(err)
	}
	msg := editorFinishedMsg{path: path, sessionID: m.sessionID, generation: m.attachmentGeneration}
	m = applyTestWorkspaceState(t, m)
	next, _ := m.Update(msg)
	m = modelValue(t, next)
	if m.textarea.Value() != "edited draft" {
		t.Fatal("workspace state discarded the edited draft")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("editor staging file was not removed")
	}
}
