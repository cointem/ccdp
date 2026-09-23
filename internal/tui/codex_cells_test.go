package tui

import (
	"github.com/charmbracelet/lipgloss"
	"strings"
	"testing"
)

func TestCodexExplorationScrollbackUsesWorkspacePaths(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.routing = &sessionRouting{generation: 7}
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.inlineMode = true
	m.workspace = "/workspace/project"
	m.turnDone = true
	m.items = []historyCell{
		{kind: "tool", messageID: "read-a", toolID: "read-a", toolName: "Read", status: "success", toolArgs: map[string]any{"file_path": "/workspace/project/a.go"}},
		{kind: "tool", messageID: "read-b", toolID: "read-b", toolName: "Read", status: "success", toolArgs: map[string]any{"file_path": "/workspace/project/b.go"}},
	}
	cmd := planTestHistory(m)
	if cmd == nil {
		t.Fatal("missing scrollback output")
	}
	msg, ok := cmd().(inlinePrintMsg)
	if !ok {
		t.Fatal("missing inline print")
	}
	text := sanitizeANSI(msg.text)
	if strings.Count(text, "Explored") != 1 || !strings.Contains(text, "Read a.go") || !strings.Contains(text, "Read b.go") || strings.Contains(text, "/workspace/") {
		t.Fatalf("invalid group: %s", text)
	}
	if m.items[0].toolArgs["file_path"] != "/workspace/project/a.go" {
		t.Fatal("render mutated arguments")
	}
}

func TestCodexCommandPreviewRetainsOutcome(t *testing.T) {
	p := toolPresentation{Name: "Bash", Status: "success", Args: map[string]any{"command": "test"}, Output: "first\nsecond\nthird\nfourth\nfifth\nsixth\nlast"}
	out := sanitizeANSI(renderToolPresentation(p, 80))
	for _, want := range []string{"Ran test", "first", "second", "+3 lines", "sixth", "last"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "third") || lipgloss.Height(out) != 6 {
		t.Fatalf("bad preview: %s", out)
	}
	p.Status = "running"
	p.Output = ""
	out = sanitizeANSI(renderToolPresentation(p, 80))
	if strings.Contains(out, "no output") || strings.Contains(out, "running") {
		t.Fatalf("premature completion: %s", out)
	}
}

func TestCodexFilePreviewRequiresConfirmedSuccess(t *testing.T) {
	p := toolPresentation{Name: "Edit", Status: "success", Args: map[string]any{"file_path": "main.go", "old_string": "old", "new_string": "new"}}
	out := sanitizeANSI(renderToolPresentation(p, 80))
	if !strings.Contains(out, "- old") || !strings.Contains(out, "+ new") {
		t.Fatalf("missing replacement: %s", out)
	}
	p.Status = "error"
	p.Output = "permission denied"
	out = sanitizeANSI(renderToolPresentation(p, 80))
	if strings.Contains(out, "+ new") || !strings.Contains(out, "permission denied") {
		t.Fatalf("failed edit shown as applied: %s", out)
	}
	p = toolPresentation{Name: "Write", Status: "success", Args: map[string]any{"content": "  preserved indent\n"}}
	out = sanitizeANSI(renderFileChangePreview(p, 80))
	if !strings.Contains(out, "  preserved indent") || strings.Contains(out, "+ ") {
		t.Fatalf("write without original must show contents, not invented diff: %q", out)
	}
}

func TestCodexCellsRespectNarrowWidth(t *testing.T) {
	for _, width := range []int{16, 40, 80} {
		for _, out := range []string{renderUserCell(strings.Repeat("中文输入", 8), width), renderAssistantCell("A paragraph with enough words to wrap onto another line.", width)} {
			for _, line := range strings.Split(out, "\n") {
				if lipgloss.Width(line) > width {
					t.Fatalf("width %d exceeded by %q", width, line)
				}
			}
		}
	}
}
