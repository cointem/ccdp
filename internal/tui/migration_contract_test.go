package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

// Keep the frontend boundary enforceable: lifecycle integration may import
// the local runtime, but rendering, input and protocol reducers must not.
func TestFrontendRuntimeAndOutputBoundaries(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	retired := map[string]bool{"syncLegacyStatus": true, "pickerState": true, "legacyAssociation": true, "pendingSubmissions": true, "pendingImages": true, "rebuildHistory": true, "appendProtocolToolProgress": true, "renderToolText": true, "markdownStream": true}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range tree.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "ccdp/internal/agent" && name != "session_host.go" {
				t.Errorf("%s imports runtime outside host adapter", name)
			}
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			if id, ok := node.(*ast.Ident); ok && retired[id.Name] {
				t.Errorf("%s resurrects retired state/path %s", name, id.Name)
			}
			if sel, ok := node.(*ast.SelectorExpr); ok {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "os" && sel.Sel.Name == "Stdout" && name != "terminal_host.go" {
					t.Errorf("%s bypasses terminal host", name)
				}
			}
			return true
		})
	}
	if _, err := os.Stat("gopher.go"); !os.IsNotExist(err) {
		t.Fatal("unused mascot renderer returned")
	}
}

func TestHistoryWaitsForEarlierMutableCell(t *testing.T) {
	m := inlineTestModel()
	m.inline.prime(nil)
	m.items = []historyCell{
		{kind: "tool", messageID: "first", toolName: "Edit", status: "queued"},
		{kind: "tool", messageID: "second", toolName: "Bash", status: "success", text: "SECOND"},
	}
	if m.planHistory() != nil {
		t.Fatal("later completed cell overtook pending cell")
	}
	m.items[0].status = "success"
	cmd := m.planHistory()
	if cmd == nil {
		t.Fatal("completed ordered batch missing")
	}
	msg := cmd().(inlinePrintMsg)
	if strings.Index(msg.text, "Edited") > strings.Index(msg.text, "Ran") {
		t.Fatal("transcript order reversed")
	}
	if !m.inline.seen(m.items[0], 0) || !m.inline.seen(m.items[1], 1) {
		t.Fatal("completion omitted a cell")
	}
}

func TestPendingHistoryReceiptPreservesExplicitStreamIdentity(t *testing.T) {
	m := inlineTestModel()
	source := "stable prefix\n"
	m.applyTranscript([]protocol.TranscriptItem{{ID: "stream", Kind: "assistant", Text: source + "tail", Status: "streaming"}})
	m.inline.prime(nil)
	planned := m.inline.clone()
	planned.offsets["message:stream"] = len(source)
	m.delivery.pending = &historyCommit{id: 7, generation: 4, next: planned}
	m.applyTranscript([]protocol.TranscriptItem{{ID: "final", PreviousID: "stream", Kind: "assistant", Text: source + "tail done", Status: "completed"}})
	if err := m.acknowledgeHistory(inlinePrintedMsg{batch: 7, generation: 4}); err != nil {
		t.Fatal(err)
	}
	if got := m.inline.offsets["message:final"]; got != len(source) {
		t.Fatalf("pending rename lost prefix: %d", got)
	}
}

func TestUnidentifiedDeltaCannotCreateSecondTranscript(t *testing.T) {
	m := inlineTestModel()
	m.applyTranscript([]protocol.TranscriptItem{{ID: "canonical", Kind: "assistant", Text: "source", Status: "streaming"}})
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventStream, Text: "unidentified duplicate"})
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].text != "source" {
		t.Fatal("raw delta altered canonical cells")
	}
}
