package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestProcessStateControlsCommandPresentation(t *testing.T) {
	p := toolPresentation{Name: "Bash", Status: "success", Args: map[string]any{"command": "go test ./..."}, Process: &protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "running"}, ProcessInfo: protocol.ProcessInfo{Background: true}, Revision: 1}}
	got := sanitizeANSI(renderToolPresentation(p, 100))
	if !strings.Contains(got, "Background") || strings.Contains(got, "Ran") {
		t.Fatal(got)
	}
	code := 1
	p.Process.Status, p.Process.ExitCode = "exited", &code
	p.Output, p.Process.Output = "Running; process_id=p.test", "compiler error"
	got = sanitizeANSI(renderToolPresentation(p, 100))
	if !strings.Contains(got, "compiler error") || !strings.Contains(got, "go test") || strings.Contains(got, "Running;") {
		t.Fatal(got)
	}
}

func TestProcessObservationsCollapseButInputAndFailureRemainVisible(t *testing.T) {
	p := toolPresentation{Name: "Process", Status: "success", Args: map[string]any{"action": "read"}, ProcessMerged: true, Process: &protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "running"}, ProcessInfo: protocol.ProcessInfo{Command: "go test ./..."}, Revision: 1}}
	if got := renderToolPresentation(p, 100); got != "" {
		t.Fatal(got)
	}
	p.Expanded = true
	if got := renderToolPresentation(p, 100); got == "" {
		t.Fatal("audit details hidden")
	}
	p.Expanded = false
	p.Args["action"] = "write"
	if got := sanitizeANSI(renderToolPresentation(p, 100)); !strings.Contains(got, "Sent input to") {
		t.Fatal(got)
	}
	p.Args["action"], p.Status = "read", "error"
	p.Output = "Process failed: observation cancelled"
	if got := sanitizeANSI(renderToolPresentation(p, 100)); !strings.Contains(got, "observation cancelled") {
		t.Fatal("failure hidden")
	}
}

func TestProcessMetadataChangesInvalidateCellCache(t *testing.T) {
	store := CellStore{confirmedItems: []historyCell{{kind: "tool", messageID: "bash", toolName: "Bash", status: "success", process: &protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "running"}, Revision: 1}}}}
	store.compose()
	first := store.items[0].revision
	store.confirmedItems[0].process = &protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: "p.test", Status: "exited"}, Revision: 2}
	store.compose()
	if store.items[0].revision == first {
		t.Fatal("completed command retained stale running render")
	}
}
