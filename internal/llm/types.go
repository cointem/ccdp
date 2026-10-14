package llm

import "encoding/json"

// ChatMessage is the wire representation of a message sent to a Chat
// Completions compatible endpoint. Content is either a plain string or a list
// of content parts (for tool-bearing messages we always use strings for the
// tool role, matching the OpenAI convention).
type ChatMessage struct {
	ProviderOrigin   string            `json:"provider_origin,omitempty"`
	ProviderWire     string            `json:"provider_wire,omitempty"`
	ProviderContent  []json.RawMessage `json:"provider_content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Role             string            `json:"role"`
	Content          any               `json:"content,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	Name             string            `json:"name,omitempty"`
}

func cloneRawBlocks(src []json.RawMessage) []json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make([]json.RawMessage, len(src))
	for i := range src {
		dst[i] = append(json.RawMessage(nil), src[i]...)
	}
	return dst
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
	PromptReported     bool                `json:"-"`
	CompletionReported bool                `json:"-"`
	PromptTokens       int                 `json:"prompt_tokens"`
	CompletionTokens   int                 `json:"completion_tokens"`
	TotalTokens        int                 `json:"total_tokens"`
	PromptDetails      *PromptTokensDetail `json:"prompt_tokens_details,omitempty"`
}

// UnmarshalJSON preserves usage field presence, including an explicit zero.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type wire Usage
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*u = Usage(value)
	u.PromptReported = reportedCounter(fields["prompt_tokens"])
	u.CompletionReported = reportedCounter(fields["completion_tokens"])
	return nil
}

func reportedCounter(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

// PromptTokensDetail carries cache-hit accounting (OpenAI-style
// prompt_tokens_details.cached_tokens).
type PromptTokensDetail struct {
	CachedTokens *int `json:"cached_tokens"`
}

// APIError is a structured provider error.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// SystemBlock is one ordered, independently-addressable section of the system
// prompt. Cacheable marks stable, session-constant sections that a caching
// wire (Anthropic cache_control) may hold across requests; dynamic sections
// (environment, memory, plan) are never cached.
type SystemBlock struct {
	Text      string
	Cacheable bool
}

// ThinkingConfig carries the requested reasoning mode for compatible providers.
type ThinkingConfig struct {
	Type string `json:"type"`
}

// CompletionRequest is the provider-neutral prepared request, including internal
// prompt metadata. Each adapter projects it into its own API request body.
type CompletionRequest struct {
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Verbosity       string          `json:"verbosity,omitempty"`
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	// SystemBlocks is the structured system prompt. chat/responses render it as
	// a single system message (joined); anthropic renders it as a system array
	// with cache_control on cacheable sections. Empty means the system message
	// in Messages is authoritative.
	SystemBlocks []SystemBlock `json:"system_blocks,omitempty"`
	Tools        []ToolDef     `json:"tools,omitempty"`
	Temperature  *float64      `json:"temperature,omitempty"`
	TopP         *float64      `json:"top_p,omitempty"`
	MaxTokens    *int          `json:"max_tokens,omitempty"`
	Stream       bool          `json:"stream"`
}

// UnmarshalArgs parses raw JSON arguments from a tool call delta.
func UnmarshalArgs(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]any{}
	}
	return m
}
