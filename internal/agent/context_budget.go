package agent

import (
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"encoding/json"
)

// needsCompact reports whether the conversation has grown past the configured
// fraction of the model's context window.
func (a *Agent) needsCompact() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.contextUsedTokensLocked() > int(float64(a.contextWindowLocked())*a.services.cfg.CompactThreshold) || historyAttachmentBytes(a.historyLocked()) > maxAttachmentBytes
}

// activeModelLocked is the request model, including a fallback binding.
func (a *Agent) activeModelLocked() string {
	if a.modelsState.activeBinding.model != "" {
		return a.modelsState.activeBinding.model
	}
	if a.modelsState.activeModel != "" {
		return a.modelsState.activeModel
	}
	if a.services.cfg != nil {
		return a.services.cfg.Model
	}
	return ""
}

func (a *Agent) contextWindowLocked() int {
	window := a.services.cfg.ContextWindowFor(a.activeModelLocked())
	if caps, ok := llm.ProviderCapabilitiesOf(a.modelsState.activeBinding.client); ok && caps.ContextWindow > 0 && (window <= 0 || caps.ContextWindow < window) {
		window = caps.ContextWindow
	}
	return window
}

func messageAttachmentBytes(message messages.Message) int64 {
	var total int64
	for _, image := range message.ImageAttachments {
		size := image.BlobSize
		if len(image.Data) > 0 {
			size = int64(len(image.Data))
		}
		total += size
	}
	return total
}

func historyAttachmentBytes(history []messages.Message) int64 {
	var total int64
	for _, message := range history {
		total += messageAttachmentBytes(message)
	}
	return total
}

// estimateTokens approximates the prompt size of the current history. When the
// provider reported a real prompt-token count (anchored at baselineLen history
// messages by recordUsage), that real number is the base and only the messages
// added since are estimated locally (Codex's BodyAfterPrefix approach) — far
// more accurate than estimating the whole history, and it implicitly accounts
// for the system prompt and tool schemas.
func (a *Agent) estimateTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	base, baseLen := a.turnMemory.tokenBaseline.promptTokens, a.turnMemory.tokenBaseline.historyLen
	if base > 0 && baseLen <= len(a.historyLocked()) {
		for _, m := range a.historyLocked()[baseLen:] {
			base += estimateMessageTokens(m)
		}
		return base
	}
	for _, m := range a.historyLocked() {
		base += estimateMessageTokens(m)
	}
	return base
}

// estimateMessageTokens estimates one history message's token footprint.
func estimateMessageTokens(m messages.Message) int {
	total := llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.ReasoningContent)
	if m.ToolUpdate != nil {
		encoded, _ := json.Marshal(m.ToolUpdate)
		total += llm.EstimateTokens(string(encoded))
	}
	for _, tc := range m.ToolCalls {
		total += llm.EstimateTokens(tc.Name + messages.MarshalArguments(tc.Arguments))
	}
	total += len(m.ImageAttachments) * 4096 // rough provider-neutral image allowance
	return total + 4                        // per-message overhead
}
