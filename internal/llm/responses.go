package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file implements the OpenAI Responses wire (POST /responses + SSE events)
// for the same Provider surface as the Chat Completions client. It is modeled
// on how compatibility layers (e.g. pi's openai-responses shared stream) map
// messages/tools to Responses items and reconcile the streamed output items —
// but kept wire-only and minimal: text, reasoning, function calls and terminal
// usage.

// responsesItem is an input item (message or function_call) and also the
// "item" carried by output_item.added / output_item.done events.
type responsesItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Output    string `json:"output,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// responsesTool is a Responses function tool declaration.
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// responsesBodyRequest is the Responses Create body we send.
type responsesBodyRequest struct {
	Model       string            `json:"model"`
	Input       []json.RawMessage `json:"input"`
	Tools       []responsesTool   `json:"tools,omitempty"`
	Stream      bool              `json:"stream"`
	Reasoning   *responsesReason  `json:"reasoning,omitempty"`
	MaxOutput   *int              `json:"max_output_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
}

type responsesReason struct {
	Effort string `json:"effort"`
}

// contentString flattens a ChatMessage content value (string or parts list)
// into plain text for Responses items that require a string (e.g. tool output).
func contentString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []ContentPart:
		var b strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	default:
		if s, ok := c.(fmt.Stringer); ok {
			return s.String()
		}
		return fmt.Sprintf("%v", c)
	}
}

func marshalItem(it responsesItem) (json.RawMessage, error) {
	b, err := json.Marshal(it)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// responsesContent translates Chat Completions parts into Responses input parts.
func responsesContent(content any) (any, error) {
	switch content.(type) {
	case nil, string:
		return content, nil
	}
	parts, ok := content.([]ContentPart)
	if !ok {
		// Prepared calls round-trip through JSON, so their parts arrive as
		// []any rather than []ContentPart. Normalize at this wire boundary.
		raw, err := json.Marshal(content)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, fmt.Errorf("llm: invalid Responses content: %w", err)
		}
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": part.Text})
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				return nil, fmt.Errorf("llm: image content requires a URL")
			}
			out = append(out, map[string]any{"type": "input_image", "image_url": part.ImageURL.URL})
		default:
			return nil, fmt.Errorf("llm: unsupported Responses content part %q", part.Type)
		}
	}
	return out, nil
}

// responsesBody builds the Responses request payload from a CompletionRequest.
func (c *Client) responsesBody(req CompletionRequest) ([]byte, error) {
	input := make([]json.RawMessage, 0, len(req.Messages)+8)
	for _, m := range req.Messages {
		if m.Role == "tool" {
			item, err := marshalItem(responsesItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: contentString(m.Content),
			})
			if err != nil {
				return nil, err
			}
			input = append(input, item)
			continue
		}
		content, err := responsesContent(m.Content)
		if err != nil {
			return nil, err
		}
		item, err := marshalItem(responsesItem{Role: m.Role, Content: content})
		if err != nil {
			return nil, err
		}
		input = append(input, item)
		// Emit an assistant's historical tool calls as function_call items so a
		// resumed conversation re-pairs calls with their outputs.
		for _, tc := range m.ToolCalls {
			fc, ferr := marshalItem(responsesItem{
				Type:      "function_call",
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments.String(),
			})
			if ferr != nil {
				return nil, ferr
			}
			input = append(input, fc)
		}
	}
	br := responsesBodyRequest{
		Model:  req.Model,
		Input:  input,
		Stream: true,
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			br.Tools = append(br.Tools, responsesTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
	}
	if req.ReasoningEffort != "" {
		br.Reasoning = &responsesReason{Effort: req.ReasoningEffort}
	}
	if req.MaxTokens != nil {
		br.MaxOutput = req.MaxTokens
	}
	br.Temperature = req.Temperature
	br.TopP = req.TopP
	return json.Marshal(br)
}

// --- SSE event shapes ---------------------------------------------------

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type responsesEvent struct {
	Type        string              `json:"type"`
	Message     string              `json:"message"`
	Delta       string              `json:"delta"`
	OutputIndex int                 `json:"output_index"`
	Arguments   string              `json:"arguments"`
	Item        *responsesItem      `json:"item"`
	Response    *responsesEventResp `json:"response"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type responsesEventResp struct {
	Status string          `json:"status"`
	Usage  *responsesUsage `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func responsesFinishReason(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	case "failed", "cancelled":
		return "error"
	default:
		return status
	}
}

// responsesStreamOnce is the Responses-wire counterpart of streamOnce. It shares
// the HTTP/retry classification (postStream) and returns the same shape so the
// owning stream loop is wire-agnostic.
func (c *Client) responsesStreamOnce(ctx context.Context, body []byte, onDelta func(string), onReasoning func(string)) (result StreamResult, retryable bool, emitted bool, retryAfter time.Duration, err error) {
	hr, retryable, retryAfter, err := c.postStream(ctx, c.baseURL+"/responses", body)
	if err != nil {
		return StreamResult{}, retryable, false, retryAfter, err
	}
	defer hr.Body.Close()

	type slot struct {
		id, name string
		args     string
	}
	toolSlots := map[int]*slot{}
	thinking := &reasoningPhase{onDelta: onDelta}
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
			break
		}
		var ev responsesEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "error":
			result.FinishReason = "error"
			return result, false, emitted, 0, fmt.Errorf("llm: provider error: %s", ev.Message)
		case "response.output_text.delta":
			emitDelta(ev.Delta)
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			emitReasoning(ev.Delta)
		case "response.reasoning_text.done", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
			thinking.end()
		case "response.function_call_arguments.delta":
			// The reasoning item is over once the call arguments stream.
			thinking.end()
			if s := toolSlots[ev.OutputIndex]; s != nil {
				s.args += ev.Delta
			}
		case "response.function_call_arguments.done":
			thinking.end()
			if s := toolSlots[ev.OutputIndex]; s != nil {
				s.args = ev.Arguments
			}
		case "response.output_item.added":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "function_call":
				// The call's arguments stream for a long time: thinking is over.
				thinking.end()
				toolSlots[ev.OutputIndex] = &slot{id: ev.Item.CallID, name: ev.Item.Name}
			case "message":
				thinking.end()
			}
		case "response.output_item.done":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "reasoning":
				thinking.end()
			case "function_call":
				s := toolSlots[ev.OutputIndex]
				if s == nil {
					s = &slot{}
					toolSlots[ev.OutputIndex] = s
				}
				if s.name == "" {
					s.name = ev.Item.Name
				}
				if ev.Item.Arguments != "" {
					s.args = ev.Item.Arguments
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			if ev.Response != nil {
				if u := ev.Response.Usage; u != nil {
					result.PromptTokens = u.InputTokens
					result.CompletionTok = u.OutputTokens
					result.CacheReported = u.InputDetails != nil && u.InputDetails.CachedTokens != nil
					result.CachedTokens = 0
					if result.CacheReported {
						result.CachedTokens = *u.InputDetails.CachedTokens
					}
				}
				result.FinishReason = responsesFinishReason(ev.Response.Status)
			}
			if ev.Type == "response.failed" || result.FinishReason == "error" || (ev.Response != nil && ev.Response.Error != nil) {
				message := "response failed"
				if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
					message = ev.Response.Error.Message
				}
				result.FinishReason = "error"
				return result, false, emitted, 0, fmt.Errorf("llm: provider error: %s", message)
			}
			if ev.Error != nil && ev.Error.Message != "" {
				return result, false, emitted, 0, fmt.Errorf("llm: provider error: %s", ev.Error.Message)
			}
			break loop
		}
	}

	// The stream can end while reasoning is still open (broken connection, or a
	// provider that sent no further boundary event): the caller must not be left
	// holding an unfinished thinking cell.
	thinking.end()

	// Materialize function calls in output_index order.
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
