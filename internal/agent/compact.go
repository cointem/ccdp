package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

// needsCompact reports whether the conversation has grown past the configured
// fraction of the model's context window.
func (a *Agent) needsCompact() bool {
	return a.estimateTokens() > int(float64(a.cfg.ContextWindow)*a.cfg.CompactThreshold)
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
	base, baseLen := a.tokenBaseline.promptTokens, a.tokenBaseline.historyLen
	if base > 0 && baseLen <= len(a.history) {
		for _, m := range a.history[baseLen:] {
			base += estimateMessageTokens(m)
		}
		return base
	}
	for _, m := range a.history {
		base += estimateMessageTokens(m)
	}
	return base
}

// estimateMessageTokens estimates one history message's token footprint.
func estimateMessageTokens(m messages.Message) int {
	total := llm.EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		total += llm.EstimateTokens(tc.Name + messages.MarshalArguments(tc.Arguments))
	}
	return total + 4 // per-message overhead
}

// compact reduces the conversation. Strategy, mirroring Claude Code's
// compaction: summarize the older portion of the history with the model,
// replace it with a structured summary, and fall back to dropping the oldest
// tool results if the LLM summarization fails or is interrupted.
func (a *Agent) compact() {
	// PreCompact hooks get a chance to veto or log before history is touched.
	if ho := a.hooks.PreCompact(a.turnCtx); ho.Decision == hooks.DecisionBlock {
		a.emitStatus("compaction blocked by hook: %s", ho.Reason)
		return
	}

	keep := a.cfg.KeepAfterCompact
	if keep < 4 {
		keep = 4
	}

	// Snapshot the spans we need before doing any model work.
	a.mu.Lock()
	if len(a.history) <= 1+keep {
		a.mu.Unlock()
		return
	}
	cut := len(a.history) - keep // messages[cut:] stays intact
	head := make([]messages.Message, 1)
	copy(head, a.history[:1])
	toSummarize := make([]messages.Message, cut-1)
	copy(toSummarize, a.history[1:cut])
	keptTail := make([]messages.Message, len(a.history)-cut)
	copy(keptTail, a.history[cut:])
	lastTs := a.history[cut-1].CreatedAt
	a.mu.Unlock()

	summary, err := a.summarize(toSummarize)
	if err != nil {
		a.emitStatus("compaction failed (%v), dropping oldest tool results", err)
		a.dropOldestToolResults()
		return
	}

	// Restore context that the model may otherwise lose (Claude Code's compact
	// restore: discovered tools, active todos, recently touched files).
	if restore := a.restoreContextSnippet(keptTail); restore != "" {
		summary += "\n\n" + restore
	}

	// Commit the replacement under the lock. The summary is injected as a user
	// message with a marker, mirroring Codex's CompactionSummary and Claude
	// Code's isCompactSummary (never a mid-conversation system message).
	a.mu.Lock()
	newHistory := make([]messages.Message, 0, 2+len(keptTail))
	newHistory = append(newHistory, head...)
	newHistory = append(newHistory, messages.Message{
		Role: messages.RoleUser,
		Content: summaryPrefix + "\n<ccdp-context-summary>\nThis is a structured summary of the earlier conversation; treat it as accurate context.\n\n" +
			summary + "\n</ccdp-context-summary>" +
			a.traceEscapeHatch(),
		CreatedAt: lastTs,
	})
	newHistory = append(newHistory, keptTail...)
	a.history = newHistory
	// The history was rewritten: the prompt-token baseline no longer maps onto
	// it, so fall back to full local estimation until the next real usage.
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()

	a.emit(Event{Type: EventCompacted})
	a.emitStatus("context compacted (kept last %d messages)", keep)
	a.evbus.Emit(events.TopicCompacted, events.MessageEvent{Role: "system", Content: summary})
	a.hooks.PostCompact(a.turnCtx)
	_ = a.Save()
}

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

// summarize asks the model to compress a span of messages into a structured
// summary. It runs synchronously and is interruptible via the turn context.
func (a *Agent) summarize(span []messages.Message) (string, error) {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation with the required section structure.\n\n--- conversation ---\n")
	inputLimit := 6000 // adaptive: longer spans get a larger per-message view
	for _, m := range span {
		head := strings.ToUpper(string(m.Role))
		if len(m.Content) > inputLimit {
			sb.WriteString(fmt.Sprintf("[%s] %s …[truncated]\n", head, m.Content[:inputLimit]))
		} else {
			sb.WriteString(fmt.Sprintf("[%s] %s\n", head, m.Content))
		}
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("[TOOLCALL %s] %s\n", tc.Name, tc.Arguments))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := llm.CompletionRequest{
		Model: a.cfg.Model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: compactSystem},
			{Role: "user", Content: sb.String()},
		},
		Stream:    false,
		MaxTokens: intPtr(4096),
	}
	res, err := a.client.Stream(ctx, req, nil)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		return "", fmt.Errorf("empty summary")
	}
	return out, nil
}

// restoreContextSnippet assembles a short "session context to restore" block
// appended to the compaction summary: discovered deferred tools, the active
// task list, and up to five files recently touched in the kept tail.
func (a *Agent) restoreContextSnippet(keptTail []messages.Message) string {
	var sb strings.Builder

	// Discovered deferred tools stay available after compaction.
	var disc []string
	for n := range a.deferTools {
		if a.isDiscovered(n) {
			disc = append(disc, n)
		}
	}
	if len(disc) > 0 {
		sort.Strings(disc)
		fmt.Fprintf(&sb, "Discovered tools available to you: %s\n", strings.Join(disc, ", "))
	}

	// Active task list (persisted across turns).
	if sec := tools.TodoSection(a.cfg.SessionDir); sec != "" {
		sb.WriteString(sec)
		sb.WriteString("\n")
	}

	// Recently touched files, from tool arguments in the kept tail.
	seen := map[string]bool{}
	var files []string
	for _, m := range keptTail {
		for _, tc := range m.ToolCalls {
			for _, key := range []string{"file_path", "path", "file"} {
				if p, ok := tc.Arguments[key].(string); ok && p != "" && !seen[p] {
					seen[p] = true
					files = append(files, p)
				}
			}
		}
	}
	if len(files) > 5 {
		files = files[:5]
	}
	if len(files) > 0 {
		fmt.Fprintf(&sb, "Recently touched files: %s\n", strings.Join(files, ", "))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// dropOldestToolResults removes old tool-result pairs to free space.
func (a *Agent) dropOldestToolResults() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// History is rewritten below; invalidate the prompt-token baseline.
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	start, cut := 1, len(a.history)
	if len(a.history) <= start+4 {
		if len(a.history) > start {
			a.history = append(a.history[:start+1], a.history[len(a.history):]...)
		}
		return
	}
	drop := 0
	for _, m := range a.history[start:cut] {
		if m.Role == messages.RoleTool {
			drop++
		}
	}
	if drop == 0 {
		// Nothing to drop; truncate the span to its first message.
		a.history = append(a.history[:start+1], a.history[cut:]...)
		return
	}
	// Remove up to half of the tool results from the span.
	remove := drop / 2
	if remove < 1 {
		remove = 1
	}
	filtered := make([]messages.Message, 0, cut-start)
	for _, m := range a.history[start:cut] {
		if m.Role == messages.RoleTool && remove > 0 {
			remove--
			continue
		}
		filtered = append(filtered, m)
	}
	a.history = append(a.history[:start], append(filtered, a.history[cut:]...)...)
	a.emit(Event{Type: EventCompacted})
	a.emitStatus("dropped oldest tool results to free context space")
}

func intPtr(n int) *int { return &n }
