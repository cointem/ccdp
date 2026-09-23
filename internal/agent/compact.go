package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

type compactFailureState struct {
	key    string
	reason string
}

// needsCompact reports whether the conversation has grown past the configured
// fraction of the model's context window.
func (a *Agent) needsCompact() bool {
	return a.estimateTokens() > int(float64(a.cfg.ContextWindowFor(a.cfg.Model))*a.cfg.CompactThreshold)
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
	total := llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		total += llm.EstimateTokens(tc.Name + messages.MarshalArguments(tc.Arguments))
	}
	return total + 4 // per-message overhead
}

// compact reduces the conversation. Strategy, mirroring Claude Code's
// compaction: summarize the older portion of the history with the model and
// replace it with a structured summary. A failed automatic attempt is paused
// for the same state so the turn cannot spin on an unavailable provider;
// explicit /compact calls remain a manual retry path.
func (a *Agent) compact() {
	a.mu.Lock()
	base := a.turnCtx
	if base == nil || base.Err() != nil {
		base = a.rootCtx
	}
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(base)
	a.mu.Lock()
	a.compactCancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.compactCancel = nil
		a.mu.Unlock()
	}()
	a.compactContextMode(ctx, false)
}

// compactContext is also used by the asynchronous command worker. Keeping the
// cancellation context explicit ensures an interrupt can abort the provider
// summarization instead of being stuck behind the command loop.
func (a *Agent) compactContext(hctx context.Context) {
	a.compactContextMode(hctx, true)
}

// compactFailureKeyLocked identifies the immutable state an automatic
// compaction attempt summarized. Include the history itself so appends,
// rewrites, and tool-result changes naturally permit a fresh attempt, plus the
// active model/settings/budget so a model or context change is an explicit
// recovery path. The caller must hold a.mu.
func (a *Agent) compactFailureKeyLocked() string {
	model := a.activeBinding.model
	if model == "" {
		model = a.activeModel
	}
	var contextWindow int
	var compactThreshold float64
	var keepAfterCompact int
	if a.cfg != nil {
		contextWindow = a.cfg.ContextWindowFor(a.cfg.Model)
		compactThreshold = a.cfg.CompactThreshold
		keepAfterCompact = a.cfg.KeepAfterCompact
	}
	return stableID("compact-state", struct {
		History          []messages.Message
		Model            string
		SettingsRevision uint64
		ContextRevision  uint64
		CatalogVersion   uint64
		ContextWindow    int
		CompactThreshold float64
		KeepAfterCompact int
		PromptTokens     int
		BaselineLen      int
	}{
		History:          a.history,
		Model:            model,
		SettingsRevision: a.settingsRev,
		ContextRevision:  a.contextRev,
		CatalogVersion:   a.catalogVersion,
		ContextWindow:    contextWindow,
		CompactThreshold: compactThreshold,
		KeepAfterCompact: keepAfterCompact,
		PromptTokens:     a.tokenBaseline.promptTokens,
		BaselineLen:      a.tokenBaseline.historyLen,
	})
}

func (a *Agent) rememberCompactFailure(key string, reason error) {
	if key == "" || reason == nil {
		return
	}
	a.mu.Lock()
	// Do not suppress a future state if another goroutine changed the history
	// or binding while the summarizer was in flight.
	if a.compactFailureKeyLocked() == key {
		a.compactFailure = &compactFailureState{key: key, reason: reason.Error()}
	}
	a.mu.Unlock()
}

func (a *Agent) compactContextMode(hctx context.Context, manual bool) {
	if hctx == nil {
		hctx = context.Background()
	}
	a.mu.Lock()
	attemptKey := a.compactFailureKeyLocked()
	if !manual && a.compactFailure != nil && a.compactFailure.key == attemptKey {
		a.mu.Unlock()
		a.emitStatus("automatic compaction paused after a provider failure; use /compact or change model to retry")
		return
	}
	a.mu.Unlock()
	// PreCompact hooks get a chance to veto or log before history is touched.
	// hookCtx: a nil turn context (turn not started yet) would panic inside
	// hooks, and an already-cancelled one would instantly "time out" them.
	if ho := a.runHookWithJournal(hctx, hooks.EventPreCompact, func(hookCtx context.Context) hooks.Output {
		return a.hooks.PreCompact(hookCtx)
	}); ho.Decision == hooks.DecisionBlock || ho.Decision == hooks.DecisionDeny {
		a.emitStatus("compaction blocked by hook: %s", ho.Reason)
		return
	}

	head, toSummarize, keptTail, lastTs, keep, ok := a.compactSpans()
	if !ok {
		return
	}

	summary, err := a.summarize(hctx, toSummarize)
	if err != nil {
		if isRequestJournalFailure(err) {
			// A prepared request is admitted only after its journal fact is
			// durable. Never hide that failure by dropping history locally or
			// trying another summarization path.
			a.emit(Event{Type: EventError, Text: "compaction journal error: " + err.Error()})
			return
		}
		if hctx.Err() != nil {
			a.emitStatus("compaction interrupted")
			return
		}
		if !manual {
			a.rememberCompactFailure(attemptKey, err)
		}
		a.emitStatus("compaction failed (%v)", err)
		// A failed summarization has not produced a confirmed replacement
		// projection. Keep the existing history byte-for-byte; dropping tool
		// results here would silently destroy recoverable context after a
		// transient provider failure.
		if manual {
			a.emitStatus("compaction kept existing history; retry after fixing the provider or context")
		} else {
			a.emitStatus("compaction kept existing history; use /compact or change model to retry")
		}
		return
	}

	a.commitCompaction(hctx, head, summary, keptTail, lastTs, keep)
}

// compactSpans snapshots the history spans needed by a compaction under one
// lock: the leading system facts (head), the region to summarize
// (toSummarize), and the newest complete turn kept verbatim (keptTail). ok is
// false when the history is already small enough that no compaction is needed.
func (a *Agent) compactSpans() (head, toSummarize, keptTail []messages.Message, lastTs time.Time, keep int, ok bool) {
	keep = a.cfg.KeepAfterCompact
	if keep < 4 {
		keep = 4
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.history) <= 1+keep {
		return nil, nil, nil, time.Time{}, keep, false
	}
	cut := len(a.history) - keep // messages[cut:] stays intact
	// Never split a tool_call↔result pair at the boundary: walk cut back past
	// any tool results whose caller would land in the summarized head.
	for cut > 1 && a.history[cut].Role == messages.RoleTool {
		cut--
	}
	// A model step is part of the user turn that produced it. Move the
	// boundary to the beginning of that turn so compaction never leaves a
	// user message or an assistant/tool round on the opposite side of its
	// own context. The newest complete turn remains in the tail even when it
	// is larger than the nominal keep count; the budget/admission layer can
	// then decide whether another compaction or an explicit user action is
	// required.
	for cut > 1 && a.history[cut].Role != messages.RoleUser {
		cut--
	}
	// History does not contain the wire system prompt. Preserve only any
	// explicit leading system facts; pinning history[0] would permanently keep
	// the first user message and make compaction ineffective for short sessions.
	headLen := 0
	for headLen < cut && a.history[headLen].Role == messages.RoleSystem {
		headLen++
	}
	head = make([]messages.Message, headLen)
	copy(head, a.history[:headLen])
	toSummarize = make([]messages.Message, cut-headLen)
	copy(toSummarize, a.history[headLen:cut])
	keptTail = make([]messages.Message, len(a.history)-cut)
	copy(keptTail, a.history[cut:])
	lastTs = a.history[cut-1].CreatedAt
	return head, toSummarize, keptTail, lastTs, keep, true
}

// commitCompaction rebuilds history as the leading facts plus a summary user
// message plus the kept tail, persists it (so an older full projection cannot
// overwrite it), resets the token baseline, and publishes the compacted events
// and post-compact hook.
func (a *Agent) commitCompaction(hctx context.Context, head []messages.Message, summary string, keptTail []messages.Message, lastTs time.Time, keep int) {
	// Restore context that the model may otherwise lose (Claude Code's compact
	// restore: discovered tools, active todos, recently touched files).
	if restore := a.restoreContextSnippet(keptTail); restore != "" {
		summary += "\n\n" + restore
	}
	// Re-attach the content of recently read files (Claude Code's
	// post-compaction file attachments) so the model keeps its working set
	// without having to re-Read everything.
	if att := a.postCompactFileAttachments(keptTail); att != "" {
		summary += "\n\n" + att
	}

	// Commit the replacement under the lock. The summary is injected as a user
	// message with a marker, mirroring Codex's CompactionSummary and Claude
	// Code's isCompactSummary (never a mid-conversation system message).
	// sanitizeToolPairs is a belt-and-braces no-op here (the cut boundary
	// never splits a pair) but keeps the invariant explicit.
	newHistory := sanitizeToolPairs(func() []messages.Message {
		nh := make([]messages.Message, 0, 2+len(keptTail))
		nh = append(nh, head...)
		nh = append(nh, messages.Message{
			Role: messages.RoleUser,
			Content: summaryPrefix + "\n<ccdp-context-summary>\nThis is a structured summary of the earlier conversation; treat it as accurate context.\n\n" +
				summary + "\n</ccdp-context-summary>" +
				a.traceEscapeHatch(),
			CreatedAt: lastTs,
		})
		nh = append(nh, keptTail...)
		return nh
	}())
	// A compaction summary is confirmed by the store before it becomes visible
	// in memory or through the event stream. This also serializes the rewrite
	// with Save so an older full projection cannot overwrite it.
	a.persistMu.Lock()
	p := a.persistenceHandle()
	var persistErr error
	if p == nil {
		persistErr = fmt.Errorf("agent: session persistence is unavailable")
	} else {
		persistErr = p.persistHistoryMessages(newHistory, fmt.Sprintf("turn-%d", a.currentTurnSeq()))
	}
	if persistErr != nil {
		a.persistMu.Unlock()
		a.failTurnPersistence(persistErr)
		return
	}
	a.mu.Lock()
	a.history = newHistory
	a.compactFailure = nil
	// The history was rewritten: the prompt-token baseline no longer maps onto
	// it, so fall back to full local estimation until the next real usage.
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.persistMu.Unlock()

	a.emit(Event{Type: EventCompacted})
	a.emitStatus("context compacted (kept last %d messages)", keep)
	a.evbus.Emit(events.TopicCompacted, events.MessageEvent{Role: "system", Content: summary})
	a.runHookWithJournal(hctx, hooks.EventPostCompact, func(hookCtx context.Context) hooks.Output {
		return a.hooks.PostCompact(hookCtx)
	})
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

// renderSpanView renders a span of messages into a bounded text view for
// summarization prompts (shared by compaction and branch summaries).
func renderSpanView(span []messages.Message, inputLimit int) string {
	var sb strings.Builder
	for _, m := range span {
		head := strings.ToUpper(string(m.Role))
		if len(m.Content) > inputLimit {
			sb.WriteString(fmt.Sprintf("[%s] %s …[truncated]\n", head, truncateUTF8Bytes(m.Content, inputLimit)))
		} else {
			sb.WriteString(fmt.Sprintf("[%s] %s\n", head, m.Content))
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
	inputLimit := 6000 // adaptive: longer spans get a larger per-message view
	a.mu.Lock()
	binding := a.activeBinding
	if binding.model == "" {
		binding.model = a.activeModel
	}
	metadata := requestJournalMetadata{
		Config:           cloneConfig(a.cfg),
		SettingsRevision: a.settingsRev,
		ContextRevision:  a.contextRev,
		CatalogVersion:   a.catalogVersion,
		Turn:             a.turnSeq,
		Step:             a.stepSeq,
		HistoryLen:       len(a.history),
	}
	a.mu.Unlock()
	if binding.client == nil {
		return "", fmt.Errorf("summary provider is unavailable")
	}
	req := llm.CompletionRequest{
		ReasoningEffort: metadata.Config.ReasoningEffort,
		Verbosity:       metadata.Config.Verbosity,
		Model:           binding.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: compactSystem},
			{Role: "user", Content: "Summarize the following conversation with the required section structure.\n\n--- conversation ---\n" + renderSpanView(span, inputLimit)},
		},
		Stream:    true,
		MaxTokens: intPtr(4096),
	}
	applyGenerationRequest(metadata.Config, &req)
	call, err := prepareProviderCall(binding, req)
	if err != nil {
		return "", err
	}
	res, err := a.streamPrepared(ctx, "compact", call, metadata, nil, nil)
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
	a.mu.Lock()
	resources := a.resources
	a.mu.Unlock()
	if resources != nil && resources.Todos != nil {
		if sec := resources.Todos.Section(); sec != "" {
			sb.WriteString(sec)
			sb.WriteString("\n")
		}
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

// postCompactFileAttachments re-reads the most recently Read files (Claude
// Code's createPostCompactFileAttachments) and renders their content into the
// compaction summary, so the model keeps its working set after the older
// conversation — which contained those Reads — is summarized away. Files whose
// content is still visible in the kept tail are skipped, as are files that
// disappeared from disk. A per-file and total byte budget keeps the
// attachments from becoming a second compaction problem.
func (a *Agent) postCompactFileAttachments(keptTail []messages.Message) string {
	// Paths still referenced by the kept tail do not need re-attachment.
	inTail := map[string]bool{}
	for _, m := range keptTail {
		for _, tc := range m.ToolCalls {
			for _, key := range []string{"file_path", "path", "file"} {
				if p, ok := tc.Arguments[key].(string); ok {
					inTail[p] = true
				}
			}
		}
	}

	const (
		maxFiles   = 5
		maxPerFile = 8000  // chars of content per file
		maxTotal   = 24000 // chars across all attachments
	)
	var (
		sb    strings.Builder
		total int
		count int
	)
	a.mu.Lock()
	resources := a.resources
	a.mu.Unlock()
	var recentReads []tools.FileReadRecord
	if resources != nil && resources.Files != nil {
		recentReads = resources.Files.RecentReads(16)
	}
	for _, rec := range recentReads {
		if count >= maxFiles || total >= maxTotal {
			break
		}
		if inTail[rec.Path] {
			continue
		}
		// FileState only records paths that were previously admitted to a Read
		// tool.  Re-check the type at the compaction boundary nevertheless: a
		// path can be replaced by a FIFO/device between the original read and
		// compaction, and opening one here could block the turn indefinitely.
		data, err := readBoundedRegularFile(rec.Path, maxPerFile+1)
		if err != nil {
			continue // deleted or unreadable since the Read
		}
		content := string(data)
		note := ""
		if info, serr := os.Stat(rec.Path); serr == nil && !info.ModTime().Equal(rec.Mtime) {
			note = " — file changed since it was read; excerpt of current content"
		}
		if len(content) > maxPerFile {
			content = truncateResult(content, maxPerFile)
		}
		if total+len(content) > maxTotal {
			content = truncateResult(content, maxTotal-total)
		}
		if content == "" && total >= maxTotal {
			break
		}
		if count == 0 {
			sb.WriteString("Post-compaction file attachments — content of files you read earlier in this conversation, re-attached so you do not have to Read them again:\n")
		}
		fmt.Fprintf(&sb, "\n<ccdp-file-attachment path=%q%s>\n%s\n</ccdp-file-attachment>\n", rec.Path, note, content)
		total += len(content) + 64
		count++
	}
	return strings.TrimRight(sb.String(), "\n")
}

// readBoundedRegularFile is intentionally local to compaction.  The regular
// file check and the +1 read happen on the same opened descriptor, so a path
// replacement cannot turn a bounded attachment read into an unbounded or
// blocking operation.  The caller applies the user-visible truncation marker
// after converting the bounded bytes to UTF-8 text.
func readBoundedRegularFile(path string, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("attachment byte limit must be positive")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment is not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, int64(maxBytes)))
}

func intPtr(n int) *int { return &n }
