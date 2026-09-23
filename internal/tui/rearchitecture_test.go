package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestHistoryToolCommitsOnlyTerminalStatus(t *testing.T) {
	for _, status := range []string{"", "queued", "pending", "starting", "running", "awaiting_approval", "unknown"} {
		m := inlineTestModel()
		m.items = []historyCell{{kind: "tool", messageID: "edit-1", toolName: "Edit", status: status}}
		m.inline.prime(m.items)
		if inlineItemComplete(m.items[0], true) {
			t.Fatalf("premature tool completion: %q", status)
		}
		if planTestHistory(m) != nil || m.inline.seen(m.items[0], 0) {
			t.Fatalf("unfinished tool committed to immutable history: %q", status)
		}
		m.items[0].status = "success"
		if planTestHistory(m) == nil || !m.inline.seen(m.items[0], 0) {
			t.Fatalf("completed tool never reached history after %q", status)
		}
	}
	for _, status := range []string{"success", "error", "denied", "cancelled", "interrupted"} {
		if !inlineItemComplete(historyCell{kind: "tool", status: status}, false) {
			t.Fatalf("terminal tool not complete: %q", status)
		}
	}
}

func TestHistoryResetRejectsPendingCommit(t *testing.T) {
	m := inlineTestModel()
	m.inline.ensure()
	planned := m.inline.clone()
	planned.printed["message:old"] = struct{}{}
	m.delivery.pending = &historyCommit{id: 1, generation: 2, next: planned}
	m.inline.forgetAll()
	if err := m.acknowledgeHistory(inlinePrintedMsg{generation: 2, batch: 1}); err != nil {
		t.Fatal(err)
	}
	if len(m.inline.printed) != 0 || m.delivery.pending != nil {
		t.Fatal("old commit resurrected cleared history")
	}
}

func TestPanelArbiterOwnsDisplayAndKeys(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("draft")
	m.picker = &selectorPanel{Selector: Selector{Title: "choose", Options: []SelectorOption{{ID: "one", Label: "one"}, {ID: "two", Label: "two"}}}}
	m.histSearch = &historySearch{}
	if m.activePanel() != panelSelector || m.manager.Target(m) != FocusSelector {
		t.Fatal("focus differs from visible panel")
	}
	_, _, handled := m.manager.Route(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !handled || m.textarea.Value() != "draft" {
		t.Fatal("panel key leaked to composer")
	}
	m.exitConfirm = true
	if m.activePanel() != panelExit || m.manager.Target(m) != FocusExit {
		t.Fatal("exit panel lost priority")
	}
	if !strings.Contains(m.bottomSurface(), "Exit ccdp?") {
		t.Fatal("wrong visible panel")
	}
}

func TestStreamControllerPartitionPreservesSource(t *testing.T) {
	source := "A paragraph.\n\n```go\n  println(\"中文\")\n```\n\nDone.\n"
	for _, step := range []int{1, 2, 3, 7, 19} {
		offset := 0
		var committed strings.Builder
		for end := step; end < len(source); end += step {
			next := (StreamController{}).CommitEnd(source[:end], offset, 40, true)
			if next < offset || next > end {
				t.Fatal("invalid commit boundary")
			}
			committed.WriteString(source[offset:next])
			offset = next
		}
		committed.WriteString(source[offset:])
		if committed.String() != source {
			t.Fatalf("partition %d lost source", step)
		}
	}
}

func TestTypedToolCellDoesNotInterpretOutputAsMetadata(t *testing.T) {
	c := historyCell{kind: "tool", toolName: "Bash", status: "success", text: "Read\n$ rm fake\nactual output", toolArgs: map[string]any{"command": "printf test"}}
	p := toolPresentationFromItem(&c)
	if p.Name != "Bash" || p.Output != c.text || p.Args["command"] != "printf test" {
		t.Fatal("output interpreted as tool metadata")
	}
	c.text = "OLD"
	a := c.cleanText()
	c.text = "NEW"
	if a == c.cleanText() {
		t.Fatal("equal-length correction reused stale cache")
	}
}

func TestCellStoreVersionsTrackCorrectionsAndReplay(t *testing.T) {
	s := CellStore{confirmedItems: []historyCell{{kind: "assistant", messageID: "a", text: "OLD"}}}
	s.compose()
	first := s.items[0].revision
	s.compose()
	if s.items[0].revision != first {
		t.Fatal("replay changed cell revision")
	}
	s.confirmedItems[0].text = "NEW"
	s.compose()
	if s.items[0].revision <= first {
		t.Fatal("equal-length correction missed revision")
	}
	s.confirmedItems = nil
	s.compose()
	if len(s.versions) != 0 {
		t.Fatal("discarded cell retained in store")
	}
}

func TestStreamBoundaryDoesNotSplitGrapheme(t *testing.T) {
	family := "👨‍👩‍👧‍👦"
	source := "ab" + family + "z"
	end := streamCommitEnd(source, 4)
	if end != len("ab"+family) {
		t.Fatalf("split family cluster at byte %d", end)
	}
	if got := streamCommitEnd("ab", 2); got != 0 {
		t.Fatalf("froze extensible last cluster: %d", got)
	}
	if got := streamCommitEnd("ae\u0301z", 2); got != len("ae\u0301") {
		t.Fatalf("split combining cluster: %d", got)
	}
}

func TestHistoryDraftReadsCommittedIDsWithoutCopyingThem(t *testing.T) {
	var ledger historyLedger
	ledger.ensure()
	item := historyCell{kind: "assistant", messageID: "old", text: "old"}
	ledger.mark(item, 0)
	draft := ledger.clone()
	if len(draft.printed) != 0 || !draft.seen(item, 0) {
		t.Fatal("draft failed to inherit committed IDs")
	}
	next := historyCell{kind: "assistant", messageID: "new", text: "new"}
	draft.mark(next, 1)
	if ledger.seen(next, 1) {
		t.Fatal("draft mutated committed history")
	}
	ledger.accept(draft)
	if !ledger.seen(item, 0) || !ledger.seen(next, 1) || ledger.inheritedPrinted != nil {
		t.Fatal("commit lost history")
	}
}
