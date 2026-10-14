package llm

import "encoding/json"

// chatBodyRequest is the Chat Completions wire envelope. Keep it separate from
// CompletionRequest: prepared calls retain internal prompt/cache metadata that
// is not an API parameter.
type chatBodyRequest struct {
	Thinking        *ThinkingConfig      `json:"thinking,omitempty"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`
	Verbosity       string               `json:"verbosity,omitempty"`
	Model           string               `json:"model"`
	Messages        []chatRequestMessage `json:"messages"`
	Tools           []ToolDef            `json:"tools,omitempty"`
	Temperature     *float64             `json:"temperature,omitempty"`
	TopP            *float64             `json:"top_p,omitempty"`
	MaxTokens       *int                 `json:"max_tokens,omitempty"`
	Stream          bool                 `json:"stream"`
	StreamOptions   *chatStreamOptions   `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequestMessage struct {
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	Role             string                `json:"role"`
	Content          any                   `json:"content,omitempty"`
	ToolCalls        []chatRequestToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string                `json:"tool_call_id,omitempty"`
	Name             string                `json:"name,omitempty"`
}

// Index belongs to streamed deltas, never to a historical request tool call.
type chatRequestToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

func (c *Client) chatBody(req CompletionRequest) ([]byte, error) {
	var streamOptions *chatStreamOptions
	if req.Stream {
		streamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	var messages []chatRequestMessage
	if req.Messages != nil {
		messages = make([]chatRequestMessage, 0, len(req.Messages))
	}
	for _, message := range req.Messages {
		wire := chatRequestMessage{ReasoningContent: message.ReasoningContent, Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID, Name: message.Name}
		for _, call := range message.ToolCalls {
			wire.ToolCalls = append(wire.ToolCalls, chatRequestToolCall{ID: call.ID, Type: call.Type, Function: call.Function})
		}
		messages = append(messages, wire)
	}
	return json.Marshal(chatBodyRequest{
		Thinking: req.Thinking, ReasoningEffort: req.ReasoningEffort, Verbosity: req.Verbosity,
		Model: req.Model, Messages: messages, Tools: req.Tools, Temperature: req.Temperature,
		TopP: req.TopP, MaxTokens: req.MaxTokens, Stream: req.Stream, StreamOptions: streamOptions,
	})
}
