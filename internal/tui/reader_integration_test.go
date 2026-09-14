package tui

import (
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func TestReaderRawEntryNavigationAndUnicodeSearch(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.Transcript = []protocol.TranscriptItem{{ID: "one", Kind: "assistant", Text: "## 第一条\n中文内容"}, {ID: "two", Kind: "assistant", Text: "## 第二条\n\x1b[2Jother"}}
	m.openTranscriptReader()
	m.readerBody(76, 8)
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyLeft})
	if body := m.readerBody(76, 8); !strings.Contains(body, "第一条") || strings.Contains(body, "第二条") {
		t.Fatalf("raw mode showed wrong entry: %q", body)
	}
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.reader.search != "中" {
		t.Fatalf("search backspace damaged Unicode: %q", m.reader.search)
	}
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.reader.index != 0 {
		t.Fatal("search failed to select matching source")
	}
}

func TestOutputReaderReplacesPagesAndCanReturn(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	m.readerSeq, m.readerRequest = 1, 1
	first := strings.Repeat("a", readerPageSize)
	m.showAgentOutput(agentOutputMsg{sessionID: m.sessionID, itemID: "tool", request: 1, page: protocol.OutputPage{Text: first, Next: readerPageSize, Total: 3 * readerPageSize, More: true}})
	if cmd := m.loadNextReaderPage(); cmd == nil {
		t.Fatal("next page unavailable")
	}
	request := m.readerRequest
	m.showAgentOutput(agentOutputMsg{sessionID: m.sessionID, itemID: "tool", request: request, offset: readerPageSize, page: protocol.OutputPage{Text: "SECOND", Next: 2 * readerPageSize, Total: 3 * readerPageSize, More: true}})
	if m.reader.sourceText() != "SECOND" {
		t.Fatal("reader accumulated previous pages instead of replacing its bounded page")
	}
	if cmd := m.loadPreviousReaderPage(); cmd == nil {
		t.Fatal("previous page unavailable")
	}
	request = m.readerRequest
	m.showAgentOutput(agentOutputMsg{sessionID: m.sessionID, itemID: "tool", request: request, offset: 0, page: protocol.OutputPage{Text: first, Next: readerPageSize, Total: 3 * readerPageSize, More: true}})
	if m.reader.sourceText() != first || len(m.reader.previousOffsets) != 0 {
		t.Fatal("previous page did not restore its source and cursor")
	}
	m.closeReader()
	m.showAgentOutput(agentOutputMsg{sessionID: m.sessionID, itemID: "tool", request: request, page: protocol.OutputPage{Text: "LATE"}})
	if m.reader != nil {
		t.Fatal("late output reopened dismissed reader")
	}
}

func TestReaderJoinsPrintBarrierAndSuspendsNativeFlush(t *testing.T) {
	m, _ := routingTestModel()
	defer m.Close()
	m.items = []logItem{{kind: "assistant", text: "ROOT-UNPRINTED", messageID: "reply"}}
	m.confirmedItems = m.items
	m.inline.prime(nil)
	m.inline.primed = true
	m.turnDone = true
	m.routing.printing = true
	if cmd := m.openTranscriptReader(); cmd != nil || !m.reader.enterPending {
		t.Fatal("reader crossed an outstanding native print")
	}
	if cmd := m.flushInline(); cmd != nil {
		t.Fatal("reader diverted root transcript into alternate-screen scrollback")
	}
	m.routing.printing = false
	m.routing.printScheduled = false
	cmd := m.nextInlinePrint()
	if cmd == nil {
		t.Fatal("completed print barrier did not admit reader")
	}
	if got := fmt.Sprintf("%T", cmd()); !strings.Contains(strings.ToLower(got), "enteraltscreen") {
		t.Fatalf("reader barrier produced %s", got)
	}
	if cmd := m.flushInline(); cmd != nil {
		t.Fatal("active reader emitted a native transcript print")
	}
	if !m.reader.screenEntered || m.reader.enterPending {
		t.Fatal("reader screen state did not settle")
	}
}
