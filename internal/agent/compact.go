package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

type compactFailureState struct {
	key    string
	reason string
}

// compact reduces the conversation. Strategy, mirroring Claude Code's
// compaction: summarize the older portion of the history with the model and
// replace it with a structured summary. A failed automatic attempt is paused
// for the same state so the turn cannot spin on an unavailable provider;
// explicit /compact calls remain a manual retry path.
func (a *Agent) compact() {
	a.mu.Lock()
	base := a.turn.turnCtx
	if base == nil {
		base = a.lifecycle.rootCtx
	}
	a.mu.Unlock()
	_ = a.compactContextMode(base, false)
}

// compactContext is also used by the asynchronous command worker. Keeping the
// cancellation context explicit ensures an interrupt can abort the provider
// summarization instead of being stuck behind the command loop.
func (a *Agent) compactContext(hctx context.Context) error {
	return a.compactContextMode(hctx, true)
}

// compactFailureKeyLocked identifies the immutable state an automatic
// compaction attempt summarized. Include the history itself so appends,
// rewrites, and tool-result changes naturally permit a fresh attempt, plus the
// active model/settings/budget so a model or context change is an explicit
// recovery path. The caller must hold a.mu.
func (a *Agent) compactFailureKeyLocked() string {
	model := a.modelsState.activeBinding.model
	if model == "" {
		model = a.modelsState.activeModel
	}
	var contextWindow int
	var compactThreshold float64
	var keepAfterCompact int
	if a.services.cfg != nil {
		contextWindow = a.contextWindowLocked()
		compactThreshold = a.services.cfg.CompactThreshold
		keepAfterCompact = a.services.cfg.KeepAfterCompact
	}
	return messages.StableID("compact-state", struct {
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
		History:          a.historyLocked(),
		Model:            model,
		SettingsRevision: a.sequences.settingsRev,
		ContextRevision:  a.sequences.contextRev,
		CatalogVersion:   a.sequences.catalogVersion,
		ContextWindow:    contextWindow,
		CompactThreshold: compactThreshold,
		KeepAfterCompact: keepAfterCompact,
		PromptTokens:     a.turnMemory.tokenBaseline.promptTokens,
		BaselineLen:      a.turnMemory.tokenBaseline.historyLen,
	})
}

func (a *Agent) compactContextMode(ctx context.Context, manual bool) error {
	step, req, err := a.snapshotContextRequest()
	if err != nil {
		return err
	}
	// Attachments can be the reason for compaction. Validate the retained
	// candidate before commit; the oversized source cannot pass that guard.
	_, err = a.reduceContext(ctx, manual, &req, false, step)
	return err
}

// reduceContext prepares a replacement off to the side and commits once.
// Neither a failed summary nor a stale source can discard the current history.
func (a *Agent) reduceContext(ctx context.Context, manual bool, req *llm.CompletionRequest, force bool, frozen ...stepRuntime) (changed bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	key := a.compactFailureKeyLocked()
	paused := !manual && a.lifecycle.compactFailure != nil && a.lifecycle.compactFailure.key == key
	history := messages.Clone(a.historyLocked())
	source := historySource(history)
	keep := max(4, a.services.cfg.KeepAfterCompact)
	window, provider := a.contextWindowLocked(), a.modelsState.activeBinding.client
	budget := window / 3
	outputCap := 0
	step := stepRuntime{cfg: cloneConfig(a.services.cfg), binding: a.modelsState.activeBinding, history: history,
		policy: a.services.sandbox.Snapshot()}
	if len(frozen) > 0 {
		step = frozen[0]
		window, provider = contextWindowForStep(step), step.binding.client
		budget = window / 3
		keep = max(4, step.cfg.KeepAfterCompact)
	}
	a.mu.Unlock()
	if len(frozen) > 0 && historySource(step.history) != source {
		return false, fmt.Errorf("conversation changed before context preparation")
	}
	if paused && !force {
		return false, nil
	}
	defer func() {
		if err != nil {
			if !manual && ctx.Err() == nil {
				a.rememberCompactFailure(key, err)
			}
			a.emitStatus("compaction kept existing history: %v", err)
		}
	}()
	if req != nil {
		requestBudget := frozenRequestBudget(provider, *req, window)
		fixed := requestBudget.SystemTokens + requestBudget.SchemaTokens + requestBudget.ReservedOutputTokens
		if window > 0 && fixed >= window {
			return false, &llm.AdmissionError{Cause: llm.ErrContextWindowExceeded, Budget: requestBudget, Limit: window,
				Detail: "system instructions, tool schemas and reserved output fill the context window; shortening history cannot make this request fit"}
		}
		// Leave room for the same instructions, schemas and generation limit
		// when choosing both the recent tail and the summary's output cap.
		available := max(1, window-fixed)
		outputCap = min(4096, max(1, available/2))
		budget = min(budget, max(1, available-outputCap-256))
	}
	if ho := a.runHookWithJournal(ctx, hooks.EventPreCompact, func(hctx context.Context) hooks.Output {
		return a.services.hooks.PreCompact(hctx)
	}); ho.Decision == hooks.DecisionBlock || ho.Decision == hooks.DecisionDeny || ho.Continue != nil && !*ho.Continue {
		return false, fmt.Errorf("compaction blocked by hook: %s", ho.Reason)
	}
	candidate, pruned := a.pruneToolOutputs(ctx, history, keep)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Cleaning is sufficient when the complete frozen request now fits its
	// target. Manual /compact still asks for a summary of earlier work.
	check := func(history []messages.Message) error {
		if req == nil {
			return nil
		}
		return a.checkContextCandidate(history, *req, step, window)
	}
	if !manual && req != nil && pruned && a.cleanedRequestFits(*req, history, candidate, force, step) {
		if err := check(candidate); err != nil {
			return false, err
		}
		if err := a.replaceHistoryFromWorker(historyReplacementPrepared{ctx: ctx, history: candidate, source: source, compact: true}); err != nil {
			return false, err
		}
		a.emit(protocol.EventView{Kind: protocol.EventStateChanged})
		a.emitStatus("older tool outputs shortened; original output remains available with ReadOutput")
		a.runHookWithJournal(ctx, hooks.EventPostCompact, func(hctx context.Context) hooks.Output { return a.services.hooks.PostCompact(hctx) })
		return true, nil
	}
	head, span, tail, lastTs, ok := compactHistorySpans(candidate, keep, budget)
	if !ok {
		return false, nil
	}
	summary, err := a.summarizeWithLimit(ctx, span, outputCap)
	if err == nil {
		err = a.commitCompactionWithOutputs(ctx, head, summary, tail, lastTs, keep, source, collectOutputReferences(span), check)
	}
	return err == nil, err
}

func compactHistorySpans(history []messages.Message, keep, budget int) (head, toSummarize, keptTail []messages.Message, lastTs time.Time, ok bool) {
	if len(history) <= 1+keep && estimateHistoryTokens(history) <= budget && historyAttachmentBytes(history) <= maxAttachmentBytes {
		return nil, nil, nil, time.Time{}, false
	}
	cut := compactCut(history, keep, budget)
	// The request has a separate raw-attachment budget. Move complete older
	// groups into the summary even when their text fits in the context window.
	if historyAttachmentBytes(history[cut:]) > maxAttachmentBytes {
		cut = compactAttachmentCut(history, cut, maxAttachmentBytes)
	}
	if cut <= 0 {
		return nil, nil, nil, time.Time{}, false
	}
	// History does not contain the wire system prompt. Preserve only any
	// explicit leading system facts; pinning history[0] would permanently keep
	// the first user message and make compaction ineffective for short sessions.
	headLen := 0
	for headLen < cut && history[headLen].Role == messages.RoleSystem {
		headLen++
	}
	head = messages.Clone(history[:headLen])
	toSummarize = messages.Clone(history[headLen:cut])
	keptTail = messages.Clone(history[cut:])
	// Preserve the latest real user input verbatim even if a long current turn
	// moved its completed tool groups into the summary.
	for i := len(history) - 1; i >= headLen; i-- {
		if history[i].Role != messages.RoleUser || history[i].InternalSource != "" || strings.HasPrefix(history[i].Content, summaryPrefix) {
			continue
		}
		if i < cut {
			keptTail = append(messages.Clone(history[i:i+1]), keptTail...)
		}
		break
	}
	lastTs = history[cut-1].CreatedAt
	return head, toSummarize, keptTail, lastTs, len(toSummarize) > 0
}

// commitCompaction rebuilds history as the leading facts plus a summary user
// message plus the kept tail, persists it (so an older full projection cannot
// overwrite it), resets the token baseline, and publishes the compacted events
// and post-compact hook.
func (a *Agent) commitCompaction(hctx context.Context, head []messages.Message, summary string, keptTail []messages.Message, lastTs time.Time, keep int, source string) error {
	return a.commitCompactionWithOutputs(hctx, head, summary, keptTail, lastTs, keep, source, nil, nil)
}

func (a *Agent) commitCompactionWithOutputs(hctx context.Context, head []messages.Message, summary string, keptTail []messages.Message, lastTs time.Time, keep int, source string, outputs []messages.OutputReference, check func([]messages.Message) error) error {
	a.mu.Lock()
	originalTokens := estimateHistoryTokens(a.historyLocked())
	originalAttachments := historyAttachmentBytes(a.historyLocked())
	a.mu.Unlock()
	// Restore context that the model may otherwise lose (Claude Code's compact
	// restore: discovered tools, active todos, recently touched files).
	if restore := a.restoreContextSnippet(keptTail); restore != "" {
		summary += "\n\n" + restore
	}

	// Commit the replacement under the lock. The summary is injected as a user
	// message with a marker, mirroring Codex's CompactionSummary and Claude
	// Code's isCompactSummary (never a mid-conversation system message).
	// compactCut admits only complete tool groups.
	newHistory := make([]messages.Message, 0, len(head)+1+len(keptTail))
	newHistory = append(newHistory, head...)
	newHistory = append(newHistory, messages.Message{
		Role: messages.RoleUser,
		Content: summaryPrefix + "\n<ccdp-context-summary>\nThis is a summary of earlier conversation. Verify uncertain details against original outputs when necessary.\n\n" +
			summary + "\n</ccdp-context-summary>" + a.traceEscapeHatch(),
		CreatedAt: lastTs, SourceOutputs: outputs,
	})
	newHistory = append(newHistory, keptTail...)
	if err := hctx.Err(); err != nil {
		return err
	}
	if estimateHistoryTokens(newHistory) >= originalTokens && historyAttachmentBytes(newHistory) >= originalAttachments {
		return fmt.Errorf("compaction did not reduce context; existing history kept")
	}
	if check != nil {
		if err := check(newHistory); err != nil {
			return err
		}
	}
	// A compaction summary is confirmed by the store before it becomes visible
	// in memory or through the event stream. This also serializes the rewrite
	// with Save so an older full projection cannot overwrite it.
	if err := a.replaceHistoryFromWorker(historyReplacementPrepared{ctx: hctx, history: newHistory, source: source, compact: true}); err != nil {
		return err
	}

	a.emit(protocol.EventView{Kind: protocol.EventStateChanged})
	a.emitStatus("context compacted (kept last %d messages)", keep)
	a.services.evbus.Emit(events.TopicCompacted, events.MessageEvent{Role: "system", Content: summary})
	a.runHookWithJournal(hctx, hooks.EventPostCompact, func(hookCtx context.Context) hooks.Output {
		return a.services.hooks.PostCompact(hookCtx)
	})
	return nil
}
