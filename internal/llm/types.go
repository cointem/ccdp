package llm

import "encoding/json"

// ChatMessage is the wire representation of a message sent to a Chat
// Completions compatible endpoint. Content is either a plain string or a list
// of content parts (for tool-bearing messages we always use strings for the
// tool role, matching the OpenAI convention).
type ChatMessage struct {
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

// ToolCall is a streaming-assembled tool call.
type ToolCall struct {
	ID       string   `json:"id"`
	Index    int      `json:"index"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function carries the tool name and (possibly partial) JSON arguments.
type Function struct {
	Name      string        `json:"name"`
	Arguments ArgumentsJSON `json:"arguments"`
}

// ArgumentsJSON is the tool-call argument payload. OpenAI sends it as a
// JSON-encoded string; some compatible providers send a raw object. We accept
// both and normalize to the raw JSON text.
type ArgumentsJSON string

// UnmarshalJSON accepts either a quoted JSON string or an inline object.
func (a *ArgumentsJSON) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = ArgumentsJSON(s)
		return nil
	}
	*a = ArgumentsJSON(string(b))
	return nil
}

// String returns the raw JSON text of the arguments.
func (a ArgumentsJSON) String() string { return string(a) }

// ToolDef declares a tool to the model.
type ToolDef struct {
	Type     string  `json:"type"`
	Function FuncDef `json:"function"`
	// PlanAllowed is runtime metadata derived from the concrete registered
	// implementation. It is intentionally omitted from provider JSON: a tool
	// name alone must never grant plan-mode capability when a plugin/custom
	// implementation shadows that name.
	PlanAllowed bool `json:"-"`
}

// FuncDef is the JSON Schema-ish definition of a function.
type FuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ContentPart is a typed content block. For images, ImageURL holds a data URL
// (base64) or http(s) URL, per the OpenAI multi-modal convention.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// StreamChunk is one decoded SSE payload line from the streaming response.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
	Error   *APIError      `json:"error,omitempty"`
}

// StreamChoice holds the delta for a single chunk.
type StreamChoice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// Delta is the incremental content for a chunk.
type Delta struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content"` // OpenAI-style chain-of-thought deltas
	ToolCalls        []ToolCall `json:"tool_calls"`
}

// Usage reports token accounting when the provider includes it.
type Usage struct {
	PromptTokens     int                 `json:"prompt_tokens"`
	CompletionTokens int                 `json:"completion_tokens"`
	TotalTokens      int                 `json:"total_tokens"`
	PromptDetails    *PromptTokensDetail `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetail carries cache-hit accounting (OpenAI-style
// prompt_tokens_details.cached_tokens).
type PromptTokensDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

// APIError is a structured provider error.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// CompletionRequest is the full request body for Chat Completions.
type ThinkingConfig struct {
	Type string `json:"type"`
}

type CompletionRequest struct {
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Verbosity       string          `json:"verbosity,omitempty"`
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	Tools           []ToolDef       `json:"tools,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Stream          bool            `json:"stream"`
}

// UnmarshalArgs parses raw JSON arguments from a tool call delta.
func UnmarshalArgs(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]any{}
	}
	return m
}
