package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"ccdp/internal/protocol"
)

func readAgentPage(t *testing.T, a *Agent, req protocol.AgentReadRequest, budget int) protocol.AgentReadPage {
	t.Helper()
	raw, err := a.supervisor.readAgent(context.Background(), req, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > budget || !utf8.ValidString(raw) {
		t.Fatalf("page violates UTF-8 byte budget: %d > %d", len(raw), budget)
	}
	var page protocol.AgentReadPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestAgentResultPagingPinsCompletedTask(t *testing.T) {
	// Larger than the removed 256 KiB result clip, with JSON escaping and
	// multibyte boundaries. Exercise durable results, not a synthetic encoder.
	report := strings.Repeat("中文😀\"\\\n<>&", 18000)
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:" + report, "text:second result"}})
	rows, err := runTestChildren(a, []childTask{{Description: "large report"}}, "read-large")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("launch: %+v %v", rows, err)
	}
	id := protocol.SessionID(rows[0].SessionID)
	page := readAgentPage(t, a, protocol.AgentReadRequest{AgentID: id}, 4096)
	if !page.More || page.PreviousTask {
		t.Fatalf("bad first page: %+v", page)
	}
	var received strings.Builder
	received.WriteString(page.Text)
	if _, err := a.supervisor.Control(context.Background(), protocol.AgentControl{ID: "next-report", SessionID: id, Action: "continue", Text: "second"}); err != nil {
		t.Fatal(err)
	}
	next, _ := a.supervisor.lookup(id)
	waitManaged(t, next)
	for page.More {
		page = readAgentPage(t, a, protocol.AgentReadRequest{AgentID: id, Cursor: page.NextCursor}, 4096)
		if !page.PreviousTask {
			t.Fatal("older result not identified after continuation")
		}
		received.WriteString(page.Text)
	}
	if received.String() != report {
		t.Fatalf("report was truncated, duplicated or switched: %d != %d bytes", received.Len(), len(report))
	}
	latest := readAgentPage(t, a, protocol.AgentReadRequest{AgentID: id}, 4096)
	if latest.Text != "second result" || latest.PreviousTask {
		t.Fatalf("default result is not latest: %+v", latest)
	}
	// A history page is a bounded index; long thinking/output remains readable
	// by item ID rather than failing the entire page.
	seen := map[string]bool{}
	req := protocol.AgentReadRequest{AgentID: id, View: "transcript"}
	foundLong := false
	for {
		page := readAgentPage(t, a, req, 2048)
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("history pagination repeated an item")
			}
			seen[item.ID] = true
			if item.Truncated && item.Kind == "assistant" {
				output := readAgentPage(t, a, protocol.AgentReadRequest{AgentID: id, View: "output", ItemID: item.ID}, 2048)
				if output.More && strings.HasPrefix(report, output.Text) {
					foundLong = true
				}
			}
		}
		if !page.More {
			break
		}
		req.Cursor = page.NextCursor
	}
	if !foundLong {
		t.Fatal("oversized history item was lost or not addressable")
	}
}

func TestAgentTextPageFitsJSONAndAdvancesUTF8Cursor(t *testing.T) {
	for _, text := range []string{"", "小", strings.Repeat("😀<>&\"\n", 1000)} {
		cursor := agentReadCursor{Agent: "child", View: "result", Run: "run"}
		var result strings.Builder
		for {
			raw, err := encodeAgentTextPage(protocol.AgentReadPage{AgentID: "child", View: "result"}, cursor, text[cursor.Offset:], false, 256)
			if err != nil || len(raw) > 256 {
				t.Fatalf("encode: bytes=%d err=%v", len(raw), err)
			}
			var page protocol.AgentReadPage
			if err := json.Unmarshal([]byte(raw), &page); err != nil {
				t.Fatal(err)
			}
			result.WriteString(page.Text)
			if !page.More {
				break
			}
			b, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
			if err != nil {
				t.Fatal(err)
			}
			before := cursor.Offset
			if err := json.Unmarshal(b, &cursor); err != nil || cursor.Offset <= before || cursor.Offset != int64(result.Len()) {
				t.Fatalf("non-progressing cursor: %+v %v", cursor, err)
			}
		}
		if result.String() != text {
			t.Fatal("text did not roundtrip")
		}
	}
}

func TestAgentFinalPageDoesNotReserveUnneededCursor(t *testing.T) {
	text := strings.Repeat("x", 100)
	raw, err := encodeAgentTextPage(protocol.AgentReadPage{AgentID: "child", View: "result"}, agentReadCursor{Agent: "child", View: "result", Run: "internal-run-identity"}, text, false, 170)
	if err != nil {
		t.Fatal(err)
	}
	var page protocol.AgentReadPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if page.More || page.Text != text || page.NextCursor != "" {
		t.Fatalf("a fitting final page was split to reserve a cursor: %+v", page)
	}
}
