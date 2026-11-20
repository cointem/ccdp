package tui

import (
	"context"
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestCollaborationCellIsNotUserOrRawJSON(t *testing.T) {
	for _, kind := range []string{"message", "final_result"} {
		text := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: kind, AgentID: "child", Name: "researcher", Text: "report body", Outcome: "succeeded"})
		cell := historyCell{kind: "collaboration", text: text}
		out := renderItemWidth(&cell, 100)
		if strings.Contains(out, "Agent collaboration data") || strings.Contains(out, `"agent_id"`) || strings.Contains(out, "❯") {
			t.Fatalf("internal envelope rendered as user data: %s", out)
		}
		if kind == "message" {
			if out != "" {
				t.Fatalf("rendered routine collaboration: %s", out)
			}
			continue
		}
		if !strings.Contains(out, "researcher") {
			t.Fatalf("missing source: %s", out)
		}
		if !strings.Contains(out, "结果已交付") || strings.Contains(out, "report body") {
			t.Fatal("completion should be compact, with body in child details")
		}
	}
}

type mailboxHistoryDirectory struct {
	protocol.SessionDirectory
	pages map[uint64]protocol.TranscriptPage
}

func (d mailboxHistoryDirectory) ReadTranscript(_ context.Context, _ protocol.SessionID, before uint64, _ int) (protocol.TranscriptPage, error) {
	return d.pages[before], nil
}

func TestAgentMailboxInLiveChatAndSavedHistory(t *testing.T) {
	m, directory := routingTestModel()
	defer m.Close()
	mail := protocol.TranscriptItem{ID: "sync", Kind: "collaboration", Text: protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", Text: "background text"})}
	send := protocol.TranscriptItem{ID: "call", Kind: "tool", Tool: "SendMessage", Status: "running", Args: []byte(`{"text":"background text"}`)}
	m.applyTranscript(nil)
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, MessageID: mail.ID, Text: mail.Text, Transcript: &mail})
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventToolStarted, Transcript: &send})
	if len(m.confirmedItems) != 0 {
		t.Fatalf("live traffic entered chat: %+v", m.confirmedItems)
	}
	send.Status, send.Text, send.Truncated = "error", "failed to send background text", true
	items := []protocol.TranscriptItem{mail, send}
	m.applyTranscript(items)
	entries := transcriptReaderEntries(&m)
	if len(entries) != 1 || entries[0].ID != "call" || strings.Contains(entries[0].Text, "background text") || entries[0].OutputMore {
		t.Fatalf("failed delivery exposed raw content: %+v", entries)
	}
	if items[1].Text != send.Text || string(items[1].Args) != string(send.Args) {
		t.Fatal("display projection modified the model transcript")
	}
	m.routing.directory = mailboxHistoryDirectory{SessionDirectory: directory, pages: map[uint64]protocol.TranscriptPage{
		0: {Items: []protocol.TranscriptItem{mail}, Before: 2, More: true},
		2: {Items: []protocol.TranscriptItem{{ID: "older", Kind: "user", Text: "earlier question"}}},
	}}
	page := m.loadAgentHistory(0)().(agentHistoryMsg)
	if !m.applyAgentHistory(page) || len(m.reader.entries) != 1 || m.reader.entries[0].ID != "older" {
		t.Fatalf("mailbox-only page masked earlier conversation: %+v", page)
	}
}
