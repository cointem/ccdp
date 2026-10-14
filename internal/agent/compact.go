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
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	key := a.compactFailureKeyLocked()
	paused := !manual && a.lifecycle.compactFailure != nil && a.lifecycle.compactFailure.key == key
	a.mu.Unlock()
	if paused {
		return nil
	}
	if ho := a.runHookWithJournal(ctx, hooks.EventPreCompact, func(hctx context.Context) hooks.Output {
		return a.services.hooks.PreCompact(hctx)
	}); ho.Decision == hooks.DecisionBlock || ho.Decision == hooks.DecisionDeny || ho.Continue != nil && !*ho.Continue {
		return fmt.Errorf("compaction blocked by hook: %s", ho.Reason)
	}
	head, span, tail, lastTs, keep, ok, source := a.compactSpans()
	if !ok {
		return nil
	}
	summary, err := a.summarize(ctx, span)
	if err == nil {
		err = a.commitCompaction(ctx, head, summary, tail, lastTs, keep, source)
	}
	if err != nil {
		if !manual && ctx.Err() == nil {
			a.rememberCompactFailure(key, err)
		}
		a.emitStatus("compaction kept existing history: %v", err)
	}
	return err
}

// compactSpans snapshots the history spans needed by a compaction under one
// lock: the leading system facts (head), the region to summarize
// (toSummarize), and complete recent tool groups kept verbatim (keptTail). ok is
// false when the history is already small enough that no compaction is needed.
func (a *Agent) compactSpans() (head, toSummarize, keptTail []messages.Message, lastTs time.Time, keep int, ok bool, source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	source = historySource(a.historyLocked())
	keep = a.services.cfg.KeepAfterCompact
	if keep < 4 {
		keep = 4
	}
	budget := a.contextWindowLocked() / 3
	if len(a.historyLocked()) <= 1+keep && estimateHistoryTokens(a.historyLocked()) <= budget && historyAttachmentBytes(a.historyLocked()) <= maxAttachmentBytes {
		return nil, nil, nil, time.Time{}, keep, false, source
	}
	cut := compactCut(a.historyLocked(), keep, budget)
	// The request has a separate raw-attachment budget. Move complete older
	// groups into the summary even when their text fits in the context window.
	if historyAttachmentBytes(a.historyLocked()[cut:]) > maxAttachmentBytes {
		cut = compactAttachmentCut(a.historyLocked(), cut, maxAttachmentBytes)
	}
	if cut <= 0 {
		return nil, nil, nil, time.Time{}, keep, false, source
	}
	// History does not contain the wire system prompt. Preserve only any
	// explicit leading system facts; pinning history[0] would permanently keep
	// the first user message and make compaction ineffective for short sessions.
	headLen := 0
	for headLen < cut && a.historyLocked()[headLen].Role == messages.RoleSystem {
		headLen++
	}
	head = messages.Clone(a.historyLocked()[:headLen])
	toSummarize = messages.Clone(a.historyLocked()[headLen:cut])
	keptTail = messages.Clone(a.historyLocked()[cut:])
	// Preserve the latest real user input verbatim even if a long current turn
	// moved its completed tool groups into the summary.
	for i := len(a.historyLocked()) - 1; i >= headLen; i-- {
		if a.historyLocked()[i].Role != messages.RoleUser || strings.HasPrefix(a.historyLocked()[i].Content, summaryPrefix) {
			continue
		}
		if i < cut {
			keptTail = append(messages.Clone(a.historyLocked()[i:i+1]), keptTail...)
		}
		break
	}
	lastTs = a.historyLocked()[cut-1].CreatedAt
	return head, toSummarize, keptTail, lastTs, keep, true, source
}

// commitCompaction rebuilds history as the leading facts plus a summary user
// message plus the kept tail, persists it (so an older full projection cannot
// overwrite it), resets the token baseline, and publishes the compacted events
// and post-compact hook.
func (a *Agent) commitCompaction(hctx context.Context, head []messages.Message, summary string, keptTail []messages.Message, lastTs time.Time, keep int, source string) error {
	a.mu.Lock()
	originalTokens := estimateHistoryTokens(a.historyLocked())
	originalAttachments := historyAttachmentBytes(a.historyLocked())
	a.mu.Unlock()
	// Restore context that the model may otherwise lose (Claude Code's compact
	// restore: discovered tools, active todos, recently touched files).
	if restore := a.restoreContextSnippet(keptTail); restore != "" {
		summary += "\n\n" + restore
	}
	// Re-attach the content of recently read files (Claude Code's
	// post-compaction file attachments) so the model keeps its working set
	// without having to re-Read everything.
	if att := a.postCompactFileAttachments(keptTail); att != "" {
		if estimateHistoryTokens(head)+estimateHistoryTokens(keptTail)+llm.EstimateTokens(summary+att) < originalTokens {
			summary += "\n\n" + att
		}
	}

	// Commit the replacement under the lock. The summary is injected as a user
	// message with a marker, mirroring Codex's CompactionSummary and Claude
	// Code's isCompactSummary (never a mid-conversation system message).
	// compactCut admits only complete tool groups.
	newHistory := make([]messages.Message, 0, len(head)+1+len(keptTail))
	newHistory = append(newHistory, head...)
	newHistory = append(newHistory, messages.Message{
		Role: messages.RoleUser,
		Content: summaryPrefix + "\n<ccdp-context-summary>\nThis is a structured summary of the earlier conversation; treat it as accurate context.\n\n" +
			summary + "\n</ccdp-context-summary>" + a.traceEscapeHatch(),
		CreatedAt: lastTs,
	})
	newHistory = append(newHistory, keptTail...)
	if err := hctx.Err(); err != nil {
		return err
	}
	if estimateHistoryTokens(newHistory) >= originalTokens && historyAttachmentBytes(newHistory) >= originalAttachments {
		return fmt.Errorf("compaction did not reduce context; existing history kept")
	}
	// A compaction summary is confirmed by the store before it becomes visible
	// in memory or through the event stream. This also serializes the rewrite
	// with Save so an older full projection cannot overwrite it.
	if err := a.replaceHistoryFromWorker(historyReplacementPrepared{ctx: hctx, history: newHistory, source: source, compact: true}); err != nil {
		return err
	}

	a.emit(Event{Type: EventCompacted})
	a.emitStatus("context compacted (kept last %d messages)", keep)
	a.services.evbus.Emit(events.TopicCompacted, events.MessageEvent{Role: "system", Content: summary})
	a.runHookWithJournal(hctx, hooks.EventPostCompact, func(hookCtx context.Context) hooks.Output {
		return a.services.hooks.PostCompact(hookCtx)
	})
	return nil
}
