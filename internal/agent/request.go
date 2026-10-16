package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

// estimateToolsTokens estimates the wire cost of the tool schemas so the
// context-usage breakdown can split total tokens into messages / tools / system.
func estimateToolsTokens(tools []llm.ToolDef) int {
	total := 0
	for _, t := range tools {
		total += llm.EstimateTokens(t.Function.Name + "\n" + t.Function.Description)
		if p, err := json.Marshal(t.Function.Parameters); err == nil {
			total += llm.EstimateTokens(string(p))
		}
		total += 12 // per-tool schema overhead
	}
	return total
}

func cloneToolDefs(src []llm.ToolDef) []llm.ToolDef {
	dst := make([]llm.ToolDef, len(src))
	for i, def := range src {
		dst[i] = def
		dst[i].Function.Parameters = cloneMap(def.Function.Parameters)
	}
	return dst
}

// filterReadOnlyTools narrows a tool list to the plan-mode safe subset.
func filterReadOnlyTools(tools []llm.ToolDef) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		if t.PlanAllowed {
			out = append(out, t)
		}
	}
	return out
}

func (a *Agent) buildRequestFromConfig(cfg config.Config, model, sys string, history []messages.Message, plan bool, frozenTools []llm.ToolDef) llm.CompletionRequest {
	// Hard guarantee: never put an unpaired tool_call / tool message on the
	// wire, whatever a history rewrite left behind (providers 400).
	history = sanitizeToolPairs(history)
	msgs := make([]llm.ChatMessage, 0, len(history)+1)
	msgs = append(msgs, llm.ChatMessage{Role: "system", Content: sys})
	var imageBytes int64

	for _, m := range history {
		switch m.Role {
		case messages.RoleSystem:
			msgs = append(msgs, llm.ChatMessage{Role: "system", Content: m.Content, ToolUpdate: messages.CloneToolUpdate(m.ToolUpdate)})
		case messages.RoleUser:
			// Multi-modal: canonical inputs use their frozen attachment blobs;
			// legacy messages retain path-based compatibility expansion.
			parts, used, hasImage, _ := a.imageContentPartsForMessage(cfg.Workspace, m, maxAttachmentBytes-imageBytes)
			if hasImage && len(parts) > 0 {
				imageBytes += used
				msgs = append(msgs, llm.ChatMessage{Role: "user", Content: parts})
				continue
			}
			msgs = append(msgs, llm.ChatMessage{Role: "user", Content: m.Content})
		case messages.RoleAssistant:
			cm := llm.ChatMessage{Role: "assistant", Content: m.Content, ProviderOrigin: m.ProviderOrigin, ProviderWire: m.ProviderWire, ProviderContent: messages.CloneProviderBlocks(m.ProviderContent)}
			if isDeepSeekModel(cfg, model) {
				cm.ReasoningContent = m.ReasoningContent
			}
			if len(m.ToolCalls) > 0 {
				cm.ToolCalls = wireToolCalls(m.ToolCalls)
			}
			msgs = append(msgs, cm)
		case messages.RoleTool:
			msgs = append(msgs, llm.ChatMessage{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    m.Content,
			})
		}
	}

	// Tool schemas are part of the same frozen step snapshot as the provider
	// binding and history. Never re-read the registry after admission.
	toolDefs := cloneToolDefs(frozenTools)

	req := llm.CompletionRequest{
		// The active model follows fallback switches and /model: the model
		// name must match the endpoint the request is sent to.
		Model:           model,
		ReasoningEffort: cfg.ReasoningEffort,
		Verbosity:       cfg.Verbosity,
		Messages:        msgs,
		Tools:           toolDefs,
		Stream:          true,
	}
	applyGenerationRequest(cfg, &req)
	if limit := cfg.MaxOutputTokensFor(model); limit > 0 {
		req.MaxTokens = intPtr(limit)
	}
	if plan {
		req.Tools = filterReadOnlyTools(req.Tools)
	}
	return req
}

func wireToolCalls(calls []messages.ToolCall) []llm.ToolCall {
	out := make([]llm.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.ToolCall{
			ID:       c.ID,
			Type:     "function",
			Function: llm.Function{Name: c.Name, Arguments: llm.ArgumentsJSON(messages.MarshalArguments(c.Arguments))},
		})
	}
	return out
}

// sanitizeToolPairs repairs tool_call↔tool-result pairing after any history
// rewrite (compact, /remove, /rewind, fork, resume of an older snapshot).
// OpenAI-compatible endpoints reject requests where an
// assistant tool_calls message is not followed by a tool result for every
// call id, or where a tool message has no preceding call — a single dangling
// pair fails EVERY subsequent request and deadlocks the session.
//
// Rules:
//   - an assistant message keeps only the calls that have results immediately
//     following it; calls without results are answered with a synthetic tool
//     result so the protocol stays intact (matching dispatchTools' interrupt
//     behavior);
//   - orphan tool results whose caller is gone are dropped;
//   - prose-only assistant messages survive untouched.
func sanitizeToolPairs(hist []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(hist))
	for i := 0; i < len(hist); {
		m := hist[i]
		if m.Role != messages.RoleAssistant || len(m.ToolCalls) == 0 {
			if m.Role == messages.RoleTool {
				// Orphan result: its caller was dropped earlier.
				i++
				continue
			}
			out = append(out, m)
			i++
			continue
		}
		// Collect the consecutive tool results following this assistant.
		callIDs := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			callIDs[tc.ID] = true
		}
		j := i + 1
		answered := make(map[string]bool, len(m.ToolCalls))
		for j < len(hist) && hist[j].Role == messages.RoleTool && callIDs[hist[j].ToolCallID] {
			answered[hist[j].ToolCallID] = true
			j++
		}
		out = append(out, m)
		out = append(out, hist[i+1:j]...)
		// Synthetic results for calls that never got one (their result was
		// dropped, or the assistant message is the last thing in history).
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, messages.NewToolResult(tc,
					"tool result unavailable after a history rewrite; execution state is unknown. Do not re-issue automatically.", true))
			}
		}
		i = j
	}
	return out
}

// truncateResult caps a tool result at maxChars, appending a marker when
// content was cut so the model knows the result was truncated.
func truncateResult(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(s) <= maxChars {
		return s
	}
	marker := fmt.Sprintf("\n…[output truncated at %d chars, %d more available]", maxChars, len(s)-maxChars)
	if len(marker) >= maxChars {
		return messages.TruncateUTF8(s, maxChars)
	}
	cut := messages.TruncateUTF8(s, maxChars-len(marker))
	return cut + marker
}

// describeHistory returns a human-readable outline of the conversation
// (used by the TUI header / session info).
func (a *Agent) describeHistory() string {
	var parts []string
	for _, m := range a.historyLocked() {
		preview := strings.ReplaceAll(m.Content, "\n", " ")
		if len(preview) > 40 {
			preview = preview[:40] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", m.Role, preview))
	}
	return strings.Join(parts, "\n")
}
