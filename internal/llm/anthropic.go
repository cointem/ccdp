package llm

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file implements the Anthropic Messages wire (POST /v1/messages + the
// Messages Events API) for the same Provider surface as the Chat Completions
// and Responses clients. It is wire-only and minimal: text, extended thinking,
// function calls and terminal usage. Auth uses the official x-api-key header.

// anthropicThinking requests extended thinking when present.
type anthropicThinking struct {
	Type string `json:"type"`
}

// anthropicTool is an Anthropic function tool declaration.
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicMessage is one entry of the top-level `messages` array.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicBodyRequest is the Messages Create payload we send. `stream` is
// always true; the adapter consumes the streaming Events API.
type anthropicBodyRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      any                `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Stream      bool               `json:"stream"`
}

// anthropicDefaultMaxTokens is the fallback when the caller did not supply a
// max_tokens; Anthropic requires it and has no meaningful default.
const anthropicDefaultMaxTokens = 4096

// blocksForContent converts a ChatMessage content value (string or parts list)
// into Anthropic content blocks (text and base64 images).
func anthropicBlocks(content any) ([]map[string]any, error) {
	if content == nil {
		return []map[string]any{{"type": "text", "text": ""}}, nil
	}
	if s, ok := content.(string); ok {
		if strings.TrimSpace(s) == "" {
			return []map[string]any{{"type": "text", "text": ""}}, nil
		}
		return []map[string]any{{"type": "text", "text": s}}, nil
	}
	parts, ok := content.([]ContentPart)
	if !ok {
		// Prepared calls round-trip through JSON, so parts arrive as []any.
		raw, err := json.Marshal(content)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, fmt.Errorf("llm: invalid Anthropic content: %w", err)
		}
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case "image", "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, fmt.Errorf("llm: image content requires a URL")
			}
			mediaType, data, err := parseDataURL(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			})
		default:
			return nil, fmt.Errorf("llm: unsupported Anthropic content part %q", p.Type)
		}
	}
	return blocks, nil
}

// applyBackboneCacheControl marks the prompt "backbone" for caching: the first
// message's leading text and the final tool_result block. This maximizes
// contiguous cache reuse across turns in a long session (Anthropic best
// practice): the evicted prefix stays cached between requests.
func applyBackboneCacheControl(messages []anthropicMessage) {
	if len(messages) == 0 {
		return
	}
	// First message: attach cache_control to its first block when it is a text
	// block, else append a cacheable text block to the leading message.
	firstContent, ok := messages[0].Content.([]map[string]any)
	if !ok || len(firstContent) == 0 {
		return
	}
	if b := firstContent[0]; b["type"] == "text" {
		b["cache_control"] = map[string]any{"type": "ephemeral"}
	} else {
		messages[0].Content = append([]map[string]any{
			{"type": "text", "text": "", "cache_control": map[string]any{"type": "ephemeral"}},
		}, firstContent...)
	}

	// Last message: if it carries a tool_result, mark it cacheable so the
	// pending tool output is part of the cached backbone.
	last, ok := messages[len(messages)-1].Content.([]map[string]any)
	if !ok {
		return
	}
	for i := len(last) - 1; i >= 0; i-- {
		if last[i]["type"] == "tool_result" {
			last[i]["cache_control"] = map[string]any{"type": "ephemeral"}
			return
		}
	}
}

// toolUseBlock builds an Anthropic tool_use content block from a tool call.
func toolUseBlock(tc ToolCall) map[string]any {
	input := json.RawMessage(tc.Function.Arguments.String())
	return map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input}
}

// anthropicBody builds the Anthropic Messages request payload.
func (c *Client) anthropicBody(req CompletionRequest) ([]byte, error) {
	var system strings.Builder
	messages := make([]anthropicMessage, 0, len(req.Messages)+4)
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(contentString(m.Content))
			continue
		case "tool":
			// Anthropic delivers tool results as `user` messages holding
			// tool_result blocks that must follow the assistant's tool_use.
			messages = append(messages, anthropicMessage{
				Role: "user",
				Content: []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     contentString(m.Content),
				}},
			})
			continue
		}
		blocks, err := anthropicBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		// Reattach historical tool calls on the assistant message so resumed
		// conversations re-pair tool_use with their tool_result blocks.
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, toolUseBlock(tc))
		}
		if len(blocks) == 0 {
			continue
		}
		messages = append(messages, anthropicMessage{Role: m.Role, Content: blocks})
	}

	maxTokens := anthropicDefaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	// Anthropic accepts system as either a plain string (fallback) or an array
	// of content blocks. When the caller supplies structured SystemBlocks we
	// emit the array and mark the stable (cacheable) sections with
	// cache_control so a long session reuses their token cost on every turn.
	var systemValue any
	if len(req.SystemBlocks) > 0 {
		sys := make([]map[string]any, 0, len(req.SystemBlocks))
		for _, b := range req.SystemBlocks {
			block := map[string]any{"type": "text", "text": b.Text}
			if b.Cacheable {
				block["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			sys = append(sys, block)
		}
		systemValue = sys
	} else if system.Len() > 0 {
		systemValue = system.String()
	}
	applyBackboneCacheControl(messages)

	br := anthropicBodyRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    systemValue,
		Messages:  messages,
		Stream:    true,
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.Function.Name == "" {
				continue
			}
			br.Tools = append(br.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}
	if req.ReasoningEffort != "" || (req.Thinking != nil && req.Thinking.Type != "") {
		br.Thinking = &anthropicThinking{Type: "enabled"}
	}
	br.Temperature = req.Temperature
	br.TopP = req.TopP
	return json.Marshal(br)
}

// parseDataURL extracts media type and base64 payload from a data URL.
func parseDataURL(raw string) (mediaType, data string, err error) {
	const prefix = "data:"
	if !strings.HasPrefix(raw, prefix) {
		return "", "", fmt.Errorf("llm: image URL is not a data URL")
	}
	rest := raw[len(prefix):]
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", fmt.Errorf("llm: malformed image data URL")
	}
	meta, _, _ = strings.Cut(meta, ";") // drop any ;charset=...
	mediaType = meta
	if payload != "" {
		// Some providers pre-decode the base64; normalize the known padding.
		if dec, err := base64.StdEncoding.DecodeString(payload); err == nil {
			data = base64.StdEncoding.EncodeToString(dec)
		} else {
			data = payload
		}
	}
	return mediaType, data, nil
}

// --- SSE event shapes ---------------------------------------------------

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Cache read/creation are pointers so an explicit zero ("the provider
	// reports no cache for this request") stays distinguishable from a payload
	// that omits the fields entirely (cache presence genuinely unknown).
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

// intValue dereferences an optional usage counter, treating an absent field as
// zero. Callers that need to tell "absent" from "explicit zero" check the
// pointer directly; callers that only sum totals use this.
func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

type anthropicStreamMessage struct {
	Usage *anthropicUsage `json:"usage,omitempty"`
}

type anthropicStreamBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type anthropicStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type anthropicErr struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicEvent struct {
	Type         string                  `json:"type"`
	Message      *anthropicStreamMessage `json:"message,omitempty"`
	Index        int                     `json:"index"`
	ContentBlock *anthropicStreamBlock   `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta   `json:"delta,omitempty"`
	Usage        *anthropicUsage         `json:"usage,omitempty"`
	StopReason   string                  `json:"stop_reason,omitempty"`
	Error        *anthropicErr           `json:"error,omitempty"`
}

func anthropicFinishReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		if s == "" {
			return s
		}
		return s
	}
}

// anthropicStreamOnce is the Messages-wire counterpart of streamOnce. It shares
// the HTTP/retry classification (postStream) and returns the same shape so the
// owning stream loop is wire-agnostic.
func (c *Client) anthropicStreamOnce(ctx context.Context, body []byte, onDelta func(string), onReasoning func(string)) (result StreamResult, retryable bool, emitted bool, retryAfter time.Duration, err error) {
	hr, retryable, retryAfter, err := c.postStream(ctx, c.baseURL+"/messages", body)
	if err != nil {
		return StreamResult{}, retryable, false, retryAfter, err
	}
	defer hr.Body.Close()

	type slot struct {
		id, name string
		args     string
	}
	toolSlots := map[int]*slot{}
	// The thinking phase ends when a non-thinking block (text/tool_use) starts,
	// when the thinking block stops, or at message_delta — claude uses the first
	// text/tool_use block start to stop its thinking mode. Each boundary closes
	// the shared reasoning phase, which emits the empty text delta that lets the
	// runtime finalize the reasoning cell instead of keeping "thinking" active
	// through the whole reply.
	thinking := &reasoningPhase{onDelta: onDelta}

	// applyUsage normalizes Anthropic accounting into the shape the rest of the
	// pipeline assumes: PromptTokens is the whole prompt (cached and uncached),
	// and CachedTokens is the cached subset of it. Anthropic reports
	// input_tokens with cache reads and writes excluded, so treating it as the
	// prompt total understates the context and makes the cached/total ratio
	// meaningless (>100%). A usage block that carries neither input nor cache
	// fields (a real message_delta reports only output_tokens) is ignored
	// instead of zeroing the totals established at message_start.
	applyUsage := func(u *anthropicUsage) {
		if u == nil {
			return
		}
		hasInput := u.InputTokens > 0 || u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil
		if hasInput {
			result.PromptTokens = u.InputTokens + intValue(u.CacheReadInputTokens) + intValue(u.CacheCreationInputTokens)
		}
		if u.CacheReadInputTokens != nil {
			result.CachedTokens = *u.CacheReadInputTokens
			result.CacheReported = true
		}
	}

	emitDelta := func(s string) {
		if s == "" {
			return
		}
		result.Text += s
		emitted = true
		thinking.markClosed()
		if onDelta != nil {
			onDelta(s)
		}
	}
	emitReasoning := func(s string) {
		if s == "" {
			return
		}
		result.Reasoning += s
		emitted = true
		thinking.start()
		if onReasoning != nil {
			onReasoning(s)
		}
	}

	scanner := bufio.NewScanner(hr.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
loop:
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return result, false, emitted, 0, ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break loop
		}
		var ev anthropicEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "error":
			message := "anthropic error"
			if ev.Error != nil && ev.Error.Message != "" {
				message = ev.Error.Message
			}
			result.FinishReason = "error"
			return result, false, emitted, 0, fmt.Errorf("llm: provider error: %s", message)
		case "message_start":
			if ev.Message != nil {
				applyUsage(ev.Message.Usage)
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				switch ev.ContentBlock.Type {
				case "thinking", "redacted_thinking":
					thinking.start()
				case "tool_use":
					// claude stops thinking mode when a tool input block starts.
					thinking.end()
					toolSlots[ev.Index] = &slot{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				}
			}
		case "content_block_stop":
			// A thinking block ending also ends the thinking phase (a subsequent
			// text/tool_use block starts in the same break).
			thinking.end()
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				emitDelta(ev.Delta.Text)
			case "thinking_delta":
				emitReasoning(ev.Delta.Thinking)
			case "input_json_delta":
				if s := toolSlots[ev.Index]; s != nil {
					s.args += ev.Delta.PartialJSON
				}
			}
		case "message_delta":
			thinking.end()
			applyUsage(ev.Usage)
			if ev.Usage != nil {
				result.CompletionTok = ev.Usage.OutputTokens
			}
			if ev.StopReason != "" {
				result.FinishReason = anthropicFinishReason(ev.StopReason)
			}
		case "message_stop":
			break loop
		}
	}

	// The stream can end while reasoning is still open (broken connection, or a
	// provider that sent no further boundary event): the caller must not be left
	// holding an unfinished thinking cell.
	thinking.end()

	// Materialize tool calls in content-block index order.
	indexes := make([]int, 0, len(toolSlots))
	for idx := range toolSlots {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		s := toolSlots[idx]
		if s.id == "" || s.name == "" {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:    s.id,
			Index: idx,
			Type:  "function",
			Function: Function{
				Name:      s.name,
				Arguments: ArgumentsJSON(s.args),
			},
		})
	}

	if serr := scanner.Err(); serr != nil {
		return result, true, emitted, 0, &RetryableError{Err: fmt.Errorf("llm: read stream: %w", serr)}
	}
	if ctx.Err() != nil {
		return result, false, emitted, 0, ctx.Err()
	}
	return result, false, emitted, 0, nil
}
