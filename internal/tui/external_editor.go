package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// editorFinishedMsg carries the temp file holding the edited draft back to the
// Model once the external editor exits and the TUI resumes.
type editorFinishedMsg struct {
	path string
	err  error
}

// externalEditor resolves the user's preferred editor from $VISUAL/$EDITOR,
// falling back to a common terminal editor. It returns the command and any extra
// arguments so callers can append the staged draft path. An empty name means no
// editor is available.
func externalEditor() (string, []string) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if fields := strings.Fields(os.Getenv(env)); len(fields) > 0 {
			return fields[0], fields[1:]
		}
	}
	for _, candidate := range []string{"vim", "vi", "nano"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", nil
}

// openExternalEditor suspends the TUI to edit the current draft in $EDITOR. The
// draft is staged in a temp file so an editor crash cannot corrupt the composer;
// the file is removed when the edit finishes. It returns nil when a modal owns
// the keys, no editor is available, or staging fails, leaving the draft intact.
func (m *Model) openExternalEditor() tea.Cmd {
	if m.question != nil || m.approval != nil || m.picker != nil || m.histSearch != nil {
		return nil
	}
	name, args := externalEditor()
	if name == "" {
		m.pushStatus("no external editor found; set $EDITOR")
		return nil
	}
	file, err := os.CreateTemp("", "ccdp-draft-*.md")
	if err != nil {
		m.pushStatus("cannot stage draft for editing: " + err.Error())
		return nil
	}
	path := file.Name()
	if _, err := file.WriteString(m.textarea.Value()); err != nil {
		file.Close()
		os.Remove(path)
		m.pushStatus("cannot stage draft for editing: " + err.Error())
		return nil
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		m.pushStatus("cannot stage draft for editing: " + err.Error())
		return nil
	}
	cmd := exec.Command(name, append(args, path)...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorFinishedMsg{path: path, err: err}
	})
}

// closeExternalEditor folds the edited draft back into the composer and removes
// the staging file. A failed editor exit keeps the original draft so work is
// never lost to a transient error.
func (m *Model) closeExternalEditor(msg editorFinishedMsg) {
	defer os.Remove(msg.path)
	if msg.err != nil {
		m.pushStatus("external editor failed: " + msg.err.Error())
		return
	}
	data, err := os.ReadFile(msg.path)
	if err != nil {
		m.pushStatus("cannot read edited draft: " + err.Error())
		return
	}
	text := strings.TrimRight(string(data), "\n")
	m.pasteFold = nil
	m.textarea.SetValue(text)
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.refreshCmdSuggest()
}
