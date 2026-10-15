package agent

import (
	"reflect"
	"testing"

	"ccdp/internal/messages"
)

func TestCloneForkHistoryCompleteBoundaries(t *testing.T) {
	user := messages.Message{Role: messages.RoleUser, Content: "investigate"}
	call := messages.Message{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "a", Name: "Read"}, {ID: "b", Name: "Rg"}}}
	a := messages.Message{Role: messages.RoleTool, ToolCallID: "a", Content: "file evidence"}
	b := messages.Message{Role: messages.RoleTool, ToolCallID: "b", Content: "search evidence"}
	pending := messages.Message{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "spawn", Name: "SpawnAgent"}}}
	for _, tt := range []struct {
		name    string
		history []messages.Message
		keep    int
	}{
		{"complete parallel results in reverse order", []messages.Message{user, call, b, a}, 4},
		{"complete round before pending spawn", []messages.Message{user, call, b, a, pending}, 4},
		{"partially completed round", []messages.Message{user, call, a}, 1},
		{"missing result before another message", []messages.Message{user, call, a, user}, 1},
		{"unknown result in round", []messages.Message{user, call, a, {Role: messages.RoleTool, ToolCallID: "unknown"}}, 1},
		{"duplicate result", []messages.Message{user, call, a, a, b}, 1},
		{"orphan result", []messages.Message{user, a}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := cloneForkHistory(tt.history)
			if !reflect.DeepEqual(got, messages.Clone(tt.history[:tt.keep])) {
				t.Fatalf("fork history = %+v, want prefix of %d messages", got, tt.keep)
			}
		})
	}
}
