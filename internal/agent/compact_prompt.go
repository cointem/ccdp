package agent

import (
	"context"
	"fmt"
	"strings"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

// summaryPrefix frames the summary so the model continues rather than restarts
// (Codex's SUMMARY_PREFIX idea).
const summaryPrefix = "Context compaction occurred: the earlier conversation was summarized below. Another agent instance (you, before compaction) had already been working on this task — continue seamlessly from the state described here rather than starting over."

// traceEscapeHatch appends the full-session trace file path to a compaction
// summary so deep historical details remain recoverable via the Read tool
// (Claude Code's transcript escape hatch).
func (a *Agent) traceEscapeHatch() string {
	path := a.TracePath()
	if path == "" {
		return ""
	}
	return "\n\n(Verbatim record of the full pre-compaction conversation: " + path +
		" — JSONL events; use the Read tool on it if you need exact historical details.)"
}

// compactSystem is the system prompt for the summarization model (Claude
// Code's structured compaction prompt, condensed to the sections that matter
// for a coding agent).
const compactSystem = `You are a context compression engine. Summarize the conversation so a
fresh agent instance can continue the work seamlessly without re-reading it.
Do NOT call any tool. Output the summary only, in the exact section structure
requested, as dense factual prose. Later parts of the conversation matter
most: prioritize recency when space runs short.

Structure the summary with EXACTLY these sections:

1. Primary Request and Intent — the user's goals and explicit constraints,
   quoting their key sentences verbatim (in their original language).
2. Decisions and Technical Concepts — technical choices made, approaches
   taken and rejected, with one-line reasons.
3. Files and Code — every file created/modified/discussed, with a note on
   what changed and WHY it mattered. Include key code identifiers.
4. Errors and Fixes — errors hit and their resolutions, especially user
   corrections and feedback (these are binding).
5. All User Messages — list EVERY user message verbatim and complete, in
   order. This section is mandatory and must not be summarized away.
6. Pending Tasks and Next Step — the explicit task list state, what was in
   progress when the conversation ended, and the concrete next step.`

// renderSpanView renders a span of messages into a bounded text view for
// summarization prompts (shared by compaction and branch summaries).
func renderSpanView(span []messages.Message, inputLimit int) string {
	var sb strings.Builder
	for _, m := range span {
		head := strings.ToUpper(string(m.Role))
		if len(m.Content) > inputLimit {
			sb.WriteString(fmt.Sprintf("[%s] %s …[truncated]\n", head, messages.TruncateUTF8(m.Content, inputLimit)))
		} else {
			sb.WriteString(fmt.Sprintf("[%s] %s\n", head, m.Content))
		}
		for _, image := range m.ImageAttachments {
			fmt.Fprintf(&sb, "[ARCHIVED IMAGE] path=%q media=%s bytes=%d blob=%s; original attachment remains in the session record.\n", image.Path, image.MediaType, messageAttachmentBytes(messages.Message{ImageAttachments: []messages.ImageAttachment{image}}), image.BlobHash)
		}
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("[TOOLCALL %s] %s\n", tc.Name, tc.Arguments))
		}
	}
	return sb.String()
}

// summarize asks the model to compress a span of messages into a structured
// summary. It runs synchronously on the given context — the live turn context
// when compacting mid-turn, so a user interrupt aborts it — and uses the
// active client/model so a fallback switch is respected.
func (a *Agent) summarize(ctx context.Context, span []messages.Message) (string, error) {
	inputLimit := 6000 // per-message excerpt limit, also bounded in total below
	a.mu.Lock()
	binding := a.modelsState.activeBinding
	if binding.model == "" {
		binding.model = a.modelsState.activeModel
	}
	metadata := requestJournalMetadata{
		Config:           cloneConfig(a.services.cfg),
		SettingsRevision: a.sequences.settingsRev,
		ContextRevision:  a.sequences.contextRev,
		CatalogVersion:   a.sequences.catalogVersion,
		Turn:             a.sequences.turnSeq,
		Step:             a.sequences.stepSeq,
		HistoryLen:       len(a.historyLocked()),
	}
	window := a.contextWindowLocked()
	a.mu.Unlock()
	if binding.client == nil {
		return "", fmt.Errorf("summary provider is unavailable")
	}
	outputLimit := min(4096, max(1, window/4))
	if n := metadata.Config.MaxOutputTokensFor(binding.model); n > 0 {
		outputLimit = min(outputLimit, n)
	}
	if caps, ok := llm.ProviderCapabilitiesOf(binding.client); ok && caps.MaxOutputTokens > 0 {
		outputLimit = min(outputLimit, caps.MaxOutputTokens)
	}
	prefix := "Summarize the following conversation with the required section structure.\n\n--- conversation ---\n"
	inputBudget := window - outputLimit - llm.EstimateTokens(compactSystem+prefix) - 256
	if inputBudget <= 0 {
		return "", fmt.Errorf("summary context budget is too small")
	}
	view := renderSpanView(span, inputLimit)
	view = truncateToTokens(view, inputBudget)
	req := llm.CompletionRequest{
		ReasoningEffort: metadata.Config.ReasoningEffort,
		Verbosity:       metadata.Config.Verbosity,
		Model:           binding.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: compactSystem},
			{Role: "user", Content: prefix + view},
		},
		Stream:    true,
		MaxTokens: intPtr(outputLimit),
	}
	applyGenerationRequest(metadata.Config, &req)
	call, err := a.prepareProviderCall(binding, req)
	if err != nil {
		return "", err
	}
	res, err := a.streamPrepared(ctx, "compact", call, metadata, nil, nil)
	if err != nil {
		return "", err
	}
	// A partial checkpoint must never replace the history it summarizes.
	// In particular, length is a successful transport outcome, not a complete
	// summary; pending tasks often appear at the end of this prompt's format.
	if res.FinishReason != "stop" || len(res.ToolCalls) != 0 {
		return "", fmt.Errorf("summary did not complete normally (finish reason %q, tool calls %d)", res.FinishReason, len(res.ToolCalls))
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		return "", fmt.Errorf("empty summary")
	}
	return out, nil
}

func intPtr(n int) *int { return &n }

// truncateToTokens bounds the total summary input rather than only each message.
func truncateToTokens(text string, budget int) string {
	if llm.EstimateTokens(text) <= budget {
		return text
	}
	marker := "\n[Earlier conversation excerpt truncated to the current model budget.]\n"
	budget -= llm.EstimateTokens(marker)
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if llm.EstimateTokens(string(runes[len(runes)-mid:])) <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return marker + string(runes[len(runes)-low:])
}
