package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func TestUnrecognizedSlashInputSubmitsLiteralMessage(t *testing.T) {
	inputs := []string{
		"//",
		"//help",
		"// we may only commit an entry from our own term\n    last := rf.lastLogIndex()\n    我看代码里还有啊",
		"  // preserve indentation\n    return false\n  ",
		"/* block comment */",
		"/unknown please explain this",
		"/unknown Bob's example",
		"/usr/bin/go test ./...",
		"/model \"unterminated",
		"/model trailing\\",
		"/\n",
	}
	for _, input := range inputs {
		for _, mode := range []struct {
			name     string
			busy     bool
			key      tea.KeyType
			strategy protocol.InputStrategy
		}{{"idle", false, tea.KeyEnter, protocol.InputSteer}, {"steer", true, tea.KeyEnter, protocol.InputSteer}, {"followup", true, tea.KeyTab, protocol.InputFollowup}} {
			t.Run(mode.name+"/"+input, func(t *testing.T) {
				m := sugModel()
				m.busy = mode.busy
				m.textarea.SetValue(input)
				m.refreshCmdSuggest()
				images := []protocol.InputImage{clipboardPNG(t)}
				m.inputImages = images
				_, cmd := m.handleKey(tea.KeyMsg{Type: mode.key})
				if cmd == nil {
					t.Fatalf("input was not submitted: %q", noticeText(m))
				}
				_ = cmd()
				client := m.client.(*recordingClient)
				if len(client.submits) != 1 || client.submits[0].Type != protocol.CommandSubmitInput {
					t.Fatalf("expected one ordinary message, got %#v", client.submits)
				}
				sent := client.submits[0].Input
				if sent.Text != input || sent.Strategy != mode.strategy || !reflect.DeepEqual(sent.Images, images) {
					t.Fatalf("message changed: %#v", sent)
				}
				if m.textarea.Value() != "" || len(m.inputImages) != 0 || len(m.history) != 1 || m.history[0] != input {
					t.Fatalf("draft/history not handled as a normal submission: %q / %#v", m.textarea.Value(), m.history)
				}
				for _, item := range m.items {
					if item.kind == "command" || item.kind == "error" {
						t.Fatalf("literal message produced command feedback: %#v", item)
					}
				}
			})
		}
	}
}

func TestUnrecognizedSlashInputRespectsDraftGuards(t *testing.T) {
	for _, guard := range []string{"disconnected", "expired images"} {
		t.Run(guard, func(t *testing.T) {
			m := sugModel()
			m.textarea.SetValue("// keep this draft")
			m.watchDisconnected = guard == "disconnected"
			m.missingHistoryImages = guard == "expired images"
			_, cmd := m.submit()
			if cmd != nil || m.textarea.Value() != "// keep this draft" || len(m.history) != 0 || noticeText(m) == "" {
				t.Fatalf("ordinary input guard bypassed: draft=%q, notice=%q", m.textarea.Value(), noticeText(m))
			}
		})
	}
}

func TestUnrecognizedSlashInputInChildSession(t *testing.T) {
	m, directory := routingTestModel()
	m = switchRoutingTest(t, m, "child")
	m.textarea.SetValue("// explain this code")
	_, cmd := m.submit()
	if cmd == nil {
		t.Fatalf("child message treated as a root command: %s", noticeText(&m))
	}
	_ = cmd()
	sent := directory.clients["child"].submits
	if len(sent) != 1 || sent[0].Input == nil || sent[0].Input.Text != "// explain this code" {
		t.Fatalf("message not routed to child: %#v", sent)
	}
}

func TestRecognizedSlashCommandsStillExecute(t *testing.T) {
	t.Run("builtin", func(t *testing.T) {
		m := sugModel()
		m.textarea.SetValue("  /MODEL \"test model\"  ")
		_, cmd := m.submit()
		if cmd == nil {
			t.Fatal("model command not scheduled")
		}
		_ = cmd()
		sent := m.client.(*recordingClient).submits
		if len(sent) != 1 || sent[0].Model == nil || sent[0].Model.Model != "test model" {
			t.Fatalf("builtin became ordinary text: %#v", sent)
		}
	})
	t.Run("alias", func(t *testing.T) {
		m := sugModel()
		m.textarea.SetValue("/?")
		m.submit()
		if len(m.client.(*recordingClient).submits) != 0 || len(m.items) != 2 || m.items[1].kind != "system" || !strings.Contains(m.items[1].text, "ccdp commands") {
			t.Fatal("help alias was not executed")
		}
	})
	t.Run("literal goal", func(t *testing.T) {
		m := sugModel()
		m.textarea.SetValue("/goal fix Bob's test")
		_, cmd := m.submit()
		if cmd == nil {
			t.Fatal("goal command not scheduled")
		}
		_ = cmd()
		sent := m.client.(*recordingClient).submits
		if len(sent) != 1 || sent[0].Goal == nil || sent[0].Goal.Objective != "fix Bob's test" {
			t.Fatalf("literal goal became ordinary text: %#v", sent)
		}
	})
	t.Run("custom", func(t *testing.T) {
		m := sugModel()
		path := filepath.Join(t.TempDir(), "triage.md")
		if err := os.WriteFile(path, []byte("Inspect $ARGUMENTS"), 0600); err != nil {
			t.Fatal(err)
		}
		m.customCmds = []customCommand{{name: "triage", path: path, source: "project"}}
		m.textarea.SetValue("/TRIAGE \"some file.go\"")
		_, cmd := m.submit()
		applyCustomCommandTeaCmd(t, m, cmd)
		sent := m.client.(*recordingClient).submits
		if len(sent) != 1 || sent[0].Input == nil || !strings.Contains(sent[0].Input.Text, "Inspect some file.go") {
			t.Fatalf("custom command not expanded: %#v", sent)
		}
	})
}

func TestSlashCompletionDoesNotCaptureCommentsOrWhitespace(t *testing.T) {
	for _, input := range []string{"//", "//help", "/\n", "/\u3000", "/model\n", "/usr/bin/go"} {
		m := sugModel()
		m.textarea.SetValue(input)
		m.refreshCmdSuggest()
		if len(m.cmdSug) != 0 {
			t.Errorf("%q opened command completion: %v", input, m.cmdSug)
		}
	}
}
