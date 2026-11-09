package agent

import (
	"context"
	"encoding/json"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

// snapshotContextRequest reads the current prompt inputs for manual/attachment
// compaction without starting a step, draining inputs, or recording a tool
// catalog update in history. A failed compaction therefore leaves history intact.
func (a *Agent) snapshotContextRequest() (stepRuntime, llm.CompletionRequest, error) {
	a.settingsCommitMu.RLock()
	a.mu.Lock()
	step := stepRuntime{cfg: cloneConfig(a.services.cfg), binding: a.modelsState.activeBinding,
		history: messages.Clone(a.historyLocked()), policy: a.services.sandbox.Snapshot(),
		sessionID: a.conversation.sessionID, sessionStartAt: a.conversation.sessionStartAt,
		skillStore: a.services.skills, isolated: a.children.childState != nil,
		modelSwitchMsg: a.conversation.modelSwitchMsg, interruptNote: a.turnMemory.interruptNote}
	if step.binding.model == "" {
		step.binding.model = a.activeModelLocked()
	}
	var lease *tools.Lease
	if a.services.registry != nil {
		lease = a.services.registry.Acquire()
	}
	a.mu.Unlock()
	if lease != nil {
		step.tools = a.toolDefsSnapshotFromLease(lease)
	}
	a.settingsCommitMu.RUnlock()
	step.goalPrompt = a.goalPrompt()
	step.todoPrompt = a.todoSection()
	if err := a.loadStepExternalContext(&step); err != nil {
		return stepRuntime{}, llm.CompletionRequest{}, err
	}
	return step, a.buildRequestForStep(step), nil
}

// prepareContextForRequest uses the same complete request accounting as
// provider admission, including instructions, schemas, images and output room.
// The caller rebuilds its frozen request if this returns true.
func (a *Agent) prepareContextForRequest(ctx context.Context, req llm.CompletionRequest, force bool, frozen ...stepRuntime) (bool, error) {
	if !force && !a.needsContextPreparation(req, frozen...) {
		return false, nil
	}
	return a.reduceContext(ctx, false, &req, force, frozen...)
}

// checkContextCandidate rebuilds only the history portion using the frozen
// step's image policy and provider settings. All other request fields remain
// byte-for-byte the captured request's values.
func (a *Agent) checkContextCandidate(history []messages.Message, req llm.CompletionRequest, step stepRuntime, window int) error {
	step.history = history
	rebuilt, err := a.buildRequestForStepChecked(step)
	if err != nil {
		return err
	}
	if len(req.Messages) == 0 || len(rebuilt.Messages) == 0 {
		return &llm.AdmissionError{Cause: llm.ErrContextWindowExceeded, Detail: "cannot rebuild a context candidate without its frozen system message"}
	}
	rebuilt.Messages[0] = req.Messages[0]
	req.Messages = rebuilt.Messages
	budget := frozenRequestBudget(step.binding.client, req, window)
	if window > 0 && budget.TotalTokens > window {
		return &llm.AdmissionError{Cause: llm.ErrContextWindowExceeded, Budget: budget, Limit: window,
			Detail: "the shortened conversation still exceeds the context window; existing history kept"}
	}
	_, err = llm.AdmitRequest(step.binding.client, req)
	return err
}

func (a *Agent) needsContextPreparation(req llm.CompletionRequest, frozen ...stepRuntime) bool {
	a.mu.Lock()
	window, threshold := a.contextWindowLocked(), a.services.cfg.CompactThreshold
	provider := a.modelsState.activeBinding.client
	attachments := historyAttachmentBytes(a.historyLocked())
	a.mu.Unlock()
	if len(frozen) > 0 {
		window, threshold, provider = contextWindowForStep(frozen[0]), frozen[0].cfg.CompactThreshold, frozen[0].binding.client
		attachments = historyAttachmentBytes(frozen[0].history)
	}
	budget := frozenRequestBudget(provider, req, window)
	if threshold <= 0 || threshold > 1 {
		threshold = .8
	}
	return window > 0 && budget.TotalTokens > int(float64(window)*threshold) || attachments > maxAttachmentBytes
}

func (a *Agent) cleanedRequestFits(req llm.CompletionRequest, old, cleaned []messages.Message, force bool, step stepRuntime) bool {
	// Only tool result text changed. Preserve every frozen request field,
	// including provider-native assistant blocks, and update results in order.
	req.Messages = append([]llm.ChatMessage(nil), req.Messages...)
	next := 0
	for i := range req.Messages {
		if req.Messages[i].Role != string(messages.RoleTool) {
			continue
		}
		for next < len(old) {
			j := next
			next++
			if old[j].Role == messages.RoleTool && old[j].ToolCallID == req.Messages[i].ToolCallID {
				req.Messages[i].Content = renderToolOutput(cleaned[j])
				break
			}
		}
	}
	window, threshold, provider := contextWindowForStep(step), step.cfg.CompactThreshold, step.binding.client
	if threshold <= 0 || threshold > 1 {
		threshold = .8
	}
	if force {
		threshold = 1
	}
	budget := frozenRequestBudget(provider, req, window)
	return window > 0 && budget.TotalTokens <= int(float64(window)*threshold) && historyAttachmentBytes(cleaned) <= maxAttachmentBytes
}

func contextWindowForStep(step stepRuntime) int {
	model := step.binding.model
	if model == "" {
		model = step.cfg.Model
	}
	window := step.cfg.ContextWindowFor(model)
	if caps, ok := llm.ProviderCapabilitiesOf(step.binding.client); ok && caps.ContextWindow > 0 && (window <= 0 || caps.ContextWindow < window) {
		window = caps.ContextWindow
	}
	return window
}

func frozenRequestBudget(provider llm.Provider, req llm.CompletionRequest, window int) llm.RequestBudget {
	caps, _ := llm.ProviderCapabilitiesOf(provider)
	if req.MaxTokens == nil && caps.ContextWindow <= 0 && window > 0 {
		reserve := min(4096, max(1, window/4))
		if caps.MaxOutputTokens > 0 {
			reserve = min(reserve, caps.MaxOutputTokens)
		}
		req.MaxTokens = &reserve
	}
	budget, _ := llm.AdmitRequest(provider, req)
	return budget
}

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
	if m.Role == messages.RoleTool {
		total += llm.EstimateTokens(renderToolOutput(m)) - llm.EstimateTokens(m.Content)
	}
	if m.ToolUpdate != nil {
		encoded, _ := json.Marshal(m.ToolUpdate)
		total += llm.EstimateTokens(string(encoded))
	}
	for _, tc := range m.ToolCalls {
		total += llm.EstimateTokens(tc.Name + messages.MarshalArguments(tc.Arguments))
	}
	total = max(total, llm.EstimateProviderContentTokens(m.ProviderContent))
	total += len(m.ImageAttachments) * 4096 // rough provider-neutral image allowance
	return total + 4                        // per-message overhead
}
