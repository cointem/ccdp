package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
)

func TestAgentGroupClicksOpenEachSessionIncludingWrappedRows(t *testing.T) {
	for _, width := range []int{40, 100} {
		m, d := routingTestModel()
		t.Cleanup(m.watchCancel)
		d.clients["second"] = &recordingClient{snapshot: protocolSnapshot("second")}
		children := []protocol.ChildSession{
			{SessionID: "child", Title: "FIRST_AGENT", Run: protocol.RunView{Status: "running"}},
			{SessionID: "second", Title: "SECOND_AGENT", Run: protocol.RunView{Status: "needs approval"}},
		}
		m.items = []historyCell{agentTaskItem("running", map[string]any{}, "", children...)}
		m.width, m.height = width, 30
		m.layout()
		rows := strings.Split(sanitizeANSI(m.View()), "\n")
		for _, entry := range []struct{ title, id string }{{"FIRST_AGENT", "child"}, {"SECOND_AGENT", "second"}} {
			y := -1
			for i, line := range rows {
				if strings.Contains(line, entry.title) {
					y = i
					break
				}
			}
			if y < 0 {
				t.Fatalf("child not visible: %s", entry.title)
			}
			cmd := clickAt(&m, 8, y)
			if cmd == nil {
				t.Fatalf("click did not open %s", entry.id)
			}
			opened, ok := cmd().(agentViewOpenedMsg)
			if !ok || opened.err != nil || string(opened.snapshot.SessionID) != entry.id {
				t.Fatalf("wrong target for %s: %+v", entry.id, opened)
			}
		}
	}
}
