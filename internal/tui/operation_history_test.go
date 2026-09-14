package tui

import (
	"fmt"
	"testing"

	"ccdp/internal/protocol"
)

func TestOperationHistoryIsBoundedWithoutDroppingActiveOwner(t *testing.T) {
	m := Model{sessionID: "session", operations: make(map[protocol.CommandID]Operation)}
	for i := 0; i < maxOperationHistory+32; i++ {
		id := protocol.CommandID(fmt.Sprintf("terminal-%03d", i))
		m.registerOperation(protocol.Command{ID: id, SessionID: "session", Type: protocol.CommandSetGeneration}, "setting")
		m.updateOperation(id, OperationApplied, protocol.Receipt{CommandID: id, SessionID: "session", Status: protocol.ReceiptApplied})
	}
	active := protocol.CommandID("active-owner")
	m.registerOperation(protocol.Command{ID: active, SessionID: "session", Type: protocol.CommandSubmitInput}, "message")
	if _, ok := m.operations[active]; !ok {
		t.Fatal("active operation was pruned")
	}
	if len(m.operations) > maxOperationHistory+1 {
		t.Fatalf("operation history grew beyond bounded terminal records: %d", len(m.operations))
	}
	if got, ok := m.operations[m.latestOperation]; !ok || got.CommandID != active || !got.Active() {
		t.Fatalf("latest active owner missing: latest=%q op=%+v present=%v", m.latestOperation, got, ok)
	}
}
