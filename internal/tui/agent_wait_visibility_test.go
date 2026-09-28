package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

func waitCell(t *testing.T, reason, status string) historyCell {
	t.Helper()
	out, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		t.Fatal(err)
	}
	return historyCell{kind: "tool", messageID: "wait", toolID: "wait-call", toolName: "WaitAgent", status: "success", text: string(out)}
}

func TestWaitAgentVisibility(t *testing.T) {
	for _, tc := range []struct {
		reason, status string
		visible        bool
	}{
		{"collaboration", "", false},
		{"user_input", "", false},
		{"no_pending_work", "", false},
		{"attention", "", true},
		{"unknown", "", true},
	} {
		t.Run(tc.reason+"/"+tc.status, func(t *testing.T) {
			item := waitCell(t, tc.reason, tc.status)
			if got := renderItemWidth(&item, 100); (got != "") != tc.visible {
				t.Fatalf("visible=%v: %q", tc.visible, got)
			}
		})
	}
	for _, status := range []string{"error", "denied", "cancelled", "interrupted"} {
		item := historyCell{kind: "tool", toolName: "WaitAgent", status: status, text: "wait diagnostic"}
		if got := renderItemWidth(&item, 100); !strings.Contains(got, "wait diagnostic") {
			t.Errorf("%s lost diagnostic: %q", status, got)
		}
	}
	item := historyCell{kind: "tool", toolName: "WaitAgent", status: "success", text: "invalid result"}
	if renderItemWidth(&item, 100) == "" {
		t.Fatal("unrecognized result hidden")
	}
}

func TestHiddenWaitPreservesTranscriptAndViewportGeometry(t *testing.T) {
	m := astraModel(t, 100, 30)
	first := historyCell{kind: "assistant", messageID: "first", text: "Before wait"}
	last := historyCell{kind: "assistant", messageID: "last", text: "After wait"}
	m.items = []historyCell{first, last}
	m.render()
	want := strings.Join(m.selGrid, "\n")
	wait := waitCell(t, "collaboration", "")
	m.items = []historyCell{first, wait, last}
	m.render()
	if got := strings.Join(m.selGrid, "\n"); got != want {
		t.Fatalf("hidden wait left gaps or shifted selection:\n%q\nwant:\n%q", got, want)
	}
	for _, target := range m.clickTargets {
		if target.id == "child" || target.id == "wait-call" {
			t.Fatalf("hidden wait has click target: %+v", target)
		}
	}
	m.confirmedItems = append([]historyCell(nil), m.items...)
	entries := transcriptReaderEntries(&m)
	if len(entries) != 3 || entries[1].ToolName != "WaitAgent" || entries[1].RawText != wait.text {
		t.Fatalf("full transcript lost wait record: %+v", entries)
	}
}

func TestHiddenWaitNativeHistoryRetainsLaterFailure(t *testing.T) {
	m := astraModel(t, 100, 30)
	wait := waitCell(t, "no_pending_work", "")
	ledger := historyLedger{}
	ledger.prime([]historyCell{wait})
	if text := ledger.plan([]historyCell{wait}, 100, false, m.renderLogItem, ""); text != "" {
		t.Fatalf("routine wait printed to scrollback: %q", text)
	}
	if !ledger.seen(wait, 0) {
		t.Fatal("hidden completed wait did not advance history")
	}
	ledger = historyLedger{}
	wait.status, wait.text = "running", ""
	ledger.prime([]historyCell{wait})
	ledger.plan([]historyCell{wait}, 100, false, m.renderLogItem, "")
	if ledger.seen(wait, 0) {
		t.Fatal("running wait was prematurely finalized")
	}
	wait.status, wait.text = "error", "wait failed"
	if text := ledger.plan([]historyCell{wait}, 100, false, m.renderLogItem, ""); !strings.Contains(text, "wait failed") {
		t.Fatalf("failure after hidden running wait disappeared: %q", text)
	}
}
