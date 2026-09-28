package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
)

func TestRegressionRollingWindowKeepsNewText(t *testing.T) {
	ledger := historyLedger{}
	ledger.prime(nil)
	render := func(c *historyCell, _ int) string { return c.text }
	old := strings.Repeat("old text\n\n", 26000)
	item := protocol.TranscriptItem{ID: "stream", Kind: "assistant", Status: "streaming", Text: old}
	first := projectTranscriptCell(item)
	_ = ledger.plan([]historyCell{first}, 80, false, render, "")
	added := "IMPORTANT_NEW_LINE\n\n" + strings.Repeat("new text\n\n", 1000)
	item.Text = old[len(added):] + added
	item.TextOffset = int64(len(added))
	second := projectTranscriptCell(item)
	emitted := ledger.plan([]historyCell{second}, 80, false, render, "")
	item.Status = "completed"
	emitted += ledger.plan([]historyCell{projectTranscriptCell(item)}, 80, true, render, "")
	if !strings.Contains(emitted, "IMPORTANT_NEW_LINE") {
		t.Fatalf("new output lost after window shifted; emitted=%q", emitted)
	}
}

func TestStreamWindowReconcilesAbsoluteCursorToFullMessage(t *testing.T) {
	ledger := historyLedger{}
	old := historyCell{messageID: "live", kind: "assistant", text: "6789abc", textOffset: 6}
	full := historyCell{messageID: "durable", kind: "assistant", text: "0123456789abcdef"}
	ledger.ensure()
	ledger.offsets[itemIdentity(old, 0)] = 11
	ledger.reconcile(old, full)
	if got := ledger.offsets[itemIdentity(full, 0)]; got != 11 {
		t.Fatalf("cursor lost on durable identity: %d", got)
	}
	shifted := historyCell{messageID: "shifted", kind: "assistant", text: "cdef", textOffset: 12}
	ledger.reconcile(old, shifted)
	if got := ledger.offsets[itemIdentity(shifted, 0)]; got != 11 {
		t.Fatalf("cursor lost when window passed emitted prefix: %d", got)
	}
}
