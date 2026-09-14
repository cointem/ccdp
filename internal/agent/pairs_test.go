package agent

import (
	"strings"
	"testing"

	"ccdp/internal/messages"
)

func call(id, name string) messages.ToolCall {
	return messages.ToolCall{ID: id, Name: name, Arguments: map[string]any{"x": "1"}}
}

func pairSanitize(t *testing.T, name string, hist, want []messages.Message) {
	t.Helper()
	got := sanitizeToolPairs(hist)
	if len(got) != len(want) {
		t.Fatalf("%s: got %d messages, want %d\n got: %+v\nwant: %+v", name, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Errorf("%s: message %d = (%s, %q), want (%s, %q)", name, i, got[i].Role, got[i].Content, want[i].Role, want[i].Content)
		}
		if len(got[i].ToolCalls) != len(want[i].ToolCalls) {
			t.Errorf("%s: message %d tool calls = %d, want %d", name, i, len(got[i].ToolCalls), len(want[i].ToolCalls))
		}
	}
}

func TestSanitizeToolPairsDropsOrphanResults(t *testing.T) {
	hist := []messages.Message{
		{Role: messages.RoleUser, Content: "hi"},
		{Role: messages.RoleAssistant, Content: "calling", ToolCalls: []messages.ToolCall{call("1", "Bash")}},
		// result for call 1 removed by a rewrite:
		{Role: messages.RoleTool, ToolCallID: "1", Content: "out"},
		{Role: messages.RoleAssistant, Content: "done"},
	}
	// drop the tool result:
	hist = append(hist[:2], hist[3])
	// The assistant keeps its call record and gets a synthetic error result
	// (mirrors dispatchTools' interrupt semantics), rather than silently
	// losing the fact that a call was attempted.
	got := sanitizeToolPairs(hist)
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4 (assistant + synthetic result)", len(got))
	}
	if got[1].Role != messages.RoleAssistant || len(got[1].ToolCalls) != 1 {
		t.Errorf("message 1 = (%s, %d calls), want assistant keeping its call", got[1].Role, len(got[1].ToolCalls))
	}
	if got[2].ToolCallID != "1" || !strings.Contains(got[2].Content, "execution state is unknown") {
		t.Errorf("message 2 = (%s, %q), want synthetic result for call 1", got[2].Role, got[2].Content)
	}
	if got[3].Role != messages.RoleAssistant || got[3].Content != "done" {
		t.Errorf("message 3 = (%s, %q), want trailing assistant preserved", got[3].Role, got[3].Content)
	}
}

func TestSanitizeToolPairsFillsMissingResults(t *testing.T) {
	hist := []messages.Message{
		{Role: messages.RoleUser, Content: "hi"},
		{Role: messages.RoleAssistant, Content: "two calls", ToolCalls: []messages.ToolCall{call("1", "Bash"), call("2", "Read")}},
		{Role: messages.RoleTool, ToolCallID: "2", Content: "file body"},
	}
	got := sanitizeToolPairs(hist)
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4 (assistant + result2 + synthetic result1)", len(got))
	}
	if got[2].ToolCallID != "2" || got[2].Content != "file body" {
		t.Errorf("message 2 = (%s, %q), want the real result for call 2", got[2].Role, got[2].Content)
	}
	if got[3].ToolCallID != "1" {
		t.Errorf("message 3 tool_call_id = %q, want synthetic result for call 1", got[3].ToolCallID)
	}
	if !strings.Contains(got[3].Content, "execution state is unknown") {
		t.Errorf("synthetic result content = %q, want unknown-state guidance", got[3].Content)
	}
}

func TestSanitizeToolPairsKeepsCompletePairs(t *testing.T) {
	hist := []messages.Message{
		{Role: messages.RoleUser, Content: "hi"},
		{Role: messages.RoleAssistant, Content: "calling", ToolCalls: []messages.ToolCall{call("1", "Bash")}},
		{Role: messages.RoleTool, ToolCallID: "1", Content: "out"},
		{Role: messages.RoleAssistant, Content: "done"},
	}
	pairSanitize(t, "complete", hist, hist)
}

func TestSanitizeToolPairsTrailingAssistantWithCalls(t *testing.T) {
	// /remove that chopped the results off the tail:
	hist := []messages.Message{
		{Role: messages.RoleUser, Content: "hi"},
		{Role: messages.RoleAssistant, Content: "calling", ToolCalls: []messages.ToolCall{call("1", "Bash")}},
	}
	got := sanitizeToolPairs(hist)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3 (assistant + synthetic result)", len(got))
	}
	if got[2].ToolCallID != "1" {
		t.Errorf("message 2 tool_call_id = %q, want 1", got[2].ToolCallID)
	}
}
