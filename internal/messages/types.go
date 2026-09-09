// Package messages defines the conversation message model shared across the
// agent core. The model is deliberately simple: roles, content, tool calls and
// tool results, with enough metadata for rendering, token accounting and
// session persistence.
package messages

import (
	"encoding/json"
	"time"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a request emitted by the model to invoke a tool.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Message is a single entry in the conversation history.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`

	// TokenCount is an estimate used for compaction decisions. It is not
	// persisted (recomputed on load).
	TokenCount int `json:"-"`
}

// NewToolResult builds a tool result message from a tool call and its output.
func NewToolResult(call ToolCall, content string, isErr bool) Message {
	if isErr {
		content = "Error: " + content
	}
	return Message{
		Role:       RoleTool,
		ToolCallID: call.ID,
		Content:    content,
		CreatedAt:  time.Now(),
	}
}

// AssistantWithTools builds an assistant message that carries tool calls.
func AssistantWithTools(content string, calls []ToolCall) Message {
	return Message{
		Role:      RoleAssistant,
		Content:   content,
		ToolCalls: calls,
		CreatedAt: time.Now(),
	}
}

// MarshalArguments serializes tool call arguments for the wire format.
func MarshalArguments(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}
