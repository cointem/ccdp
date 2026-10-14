package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// Agent lineage accessors (guarded by mu). Set by Fork; persisted in the session log.
// parentID is the session this one branched from, branchPoint the history
// index the branch starts at, branchSummary an LLM summary of the abandoned
// direction so the new branch knows what was already tried.
func (a *Agent) Lineage() (parentID string, branchPoint int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conversation.parentID, a.conversation.branchPoint
}

// branchSummarySystem is the summarization prompt for an abandoned branch:
// unlike compaction, the goal is "what was tried so it is not repeated".
const branchSummarySystem = `You summarize an abandoned direction of work so a fresh
agent branch knows what was already tried without repeating it. In dense
factual prose: the goal, what was done and attempted, and the outcome or
blockers. Do NOT call any tool. Output the summary only.`

// summarizeBranchFrozen condenses the abandoned suffix with one immutable
// provider binding and the source root context captured by Fork. Keeping the
// provider and model together prevents a concurrent model switch from mixing
// request metadata with the wrong provider. It uses the same prepared-call and
// journal boundary as main turns and compaction.
func (a *Agent) summarizeBranchFrozen(span []messages.Message, binding modelBinding, rootCtx context.Context, metadata requestJournalMetadata) (string, error) {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	if binding.client == nil {
		return "", errors.New("agent: fork summary provider is unavailable")
	}
	ctx, cancel := context.WithTimeout(rootCtx, 2*time.Minute)
	defer cancel()
	req := llm.CompletionRequest{
		ReasoningEffort: metadata.Config.ReasoningEffort,
		Verbosity:       metadata.Config.Verbosity,
		Model:           binding.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: branchSummarySystem},
			{Role: "user", Content: "Summarize this abandoned direction of work.\n\n--- conversation ---\n" + renderSpanView(span, 4000)},
		},
		Stream:    true,
		MaxTokens: intPtr(1024),
	}
	applyGenerationRequest(metadata.Config, &req)
	call, err := a.prepareProviderCall(binding, req)
	if err != nil {
		return "", err
	}
	res, err := a.streamPrepared(ctx, "fork", call, metadata, nil, nil)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		return "", fmt.Errorf("empty branch summary")
	}
	return out, nil
}

// Fork branches a new session from this one (pi's session tree): history[:keep]
// is copied into a fresh JSONL session and the abandoned suffix is summarized
// by the model. Fork never switches the source Agent; callers can explicitly
// open the returned child later once the UI is ready to replace its handle.
func (a *Agent) Fork(keep int) (string, error) {
	a.mu.Lock()
	busy, closing := a.turn.busy, a.lifecycle.closing || a.lifecycle.closed
	a.mu.Unlock()
	if busy {
		return "", fmt.Errorf("cannot fork while a turn is running")
	}
	if closing {
		return "", fmt.Errorf("cannot fork while session is closing")
	}

	oldID, sourceCfg, prefix, suffix, usage, sourceBinding, rootCtx, metadata, keep := a.snapshotForkSource(keep)
	a.mu.Lock()
	childGoal := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()

	// Summarization is best effort for provider errors, but cancellation of the
	// source root context is a hard stop: do not publish a child with a silently
	// missing summary after the caller has begun shutdown.
	summary := ""
	if len(suffix) > 0 {
		if s, err := a.summarizeBranchFrozen(suffix, sourceBinding, rootCtx, metadata); err == nil {
			summary = s
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (rootCtx != nil && rootCtx.Err() != nil) {
			return "", fmt.Errorf("summarize fork: %w", err)
		} else if isRequestJournalFailure(err) {
			// Journal admission/finish is part of the fork operation's
			// correctness boundary. Do not create a branch after either fact
			// failed, even though ordinary provider summary errors remain best
			// effort when no summary is available.
			return "", fmt.Errorf("summarize fork journal: %w", err)
		} else {
			a.logv("branch summary failed: %v", err)
		}
	}
	if rootCtx != nil {
		if err := rootCtx.Err(); err != nil {
			return "", fmt.Errorf("fork canceled: %w", err)
		}
	}

	newID := newSessionID()
	newCreatedAt := time.Now().UTC()
	if childGoal != nil {
		if childGoal.PendingObjective != "" {
			childGoal.Objective, childGoal.PendingObjective = childGoal.PendingObjective, ""
		}
		childGoal.ID = "goal-" + newID
		childGoal.Status = protocol.GoalPaused
		childGoal.Reason = "Forked goal; resume explicitly to continue."
		childGoal.TokensUsed, childGoal.ModelCalls, childGoal.RunModelCalls = 0, 0, 0
		childGoal.ActiveTimeSeconds, childGoal.TokensEstimated = 0, false
		childGoal.CreatedAt, childGoal.UpdatedAt = newCreatedAt, newCreatedAt
	}
	childCfg := sourceCfg
	childCfg.SessionID = newID
	childCfg, publish, cleanup, err := stageHistorySession(rootCtx, childCfg)
	if err != nil {
		return "", err
	}
	defer cleanup()
	childPersistence, err := openSessionPersistence(&childCfg, newID, newCreatedAt)
	if err != nil {
		return "", fmt.Errorf("open branched session: %w", err)
	}
	closeChild := func(operationErr error) error {
		closeErr := childPersistence.Close()
		if operationErr != nil {
			return errors.Join(operationErr, closeErr)
		}
		return closeErr
	}
	// The fork point can split a tool_call↔result pair (keep may land between
	// an assistant tool_calls message and its results); sanitize the frozen
	// child history so its first provider request is valid.
	branch := buildForkBranch(prefix, summary, oldID, keep, newCreatedAt)
	branch, err = copyHistory(rootCtx, a.persistenceHandle(), childPersistence, branch)
	if err != nil {
		return "", closeChild(err)
	}

	if _, err := childPersistence.CommitEvents("fork-"+newID, session.SessionImported{
		SessionID: newID, FormatVersion: session.SchemaVersion, Source: "fork", OriginalPath: oldID,
		ImportedAt: newCreatedAt, ParentID: oldID, BranchPoint: keep, BranchSummary: summary,
	}); err != nil {
		return "", closeChild(fmt.Errorf("persist branch metadata: %w", err))
	}
	if err := childPersistence.InitializeHistory(rootCtx, branch, "fork-"+newID); err != nil {
		return "", closeChild(fmt.Errorf("persist branch history: %w", err))
	}
	if err := childPersistence.persistUsage(usage); err != nil {
		return "", closeChild(fmt.Errorf("persist branch usage: %w", err))
	}
	if err := childPersistence.PersistGoal(childGoal); err != nil {
		return "", closeChild(fmt.Errorf("persist branch goal: %w", err))
	}
	if sourceCfg.NoSessionPersistence {
		if err := retainMemoryPersistence(a.memorySessionCatalog(), childPersistence); err != nil {
			return "", closeChild(err)
		}
	}
	if err := childPersistence.Close(); err != nil {
		return "", fmt.Errorf("close branched session: %w", err)
	}
	if err := publish(); err != nil {
		return "", fmt.Errorf("publish branched session: %w", err)
	}
	return newID, nil
}

// snapshotForkSource captures the immutable source-session state a branch needs
// under one lock: identity, config, split history spans, usage, binding, root
// context, and the request-journal metadata. keep is clamped to the history
// bounds.
func (a *Agent) snapshotForkSource(keep int) (oldID string, sourceCfg config.Config, prefix, suffix []messages.Message, usage Usage, sourceBinding modelBinding, rootCtx context.Context, metadata requestJournalMetadata, clampedKeep int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	oldID = a.conversation.sessionID
	sourceCfg = cloneConfig(a.services.cfg)
	n := len(a.historyLocked())
	if keep < 0 || keep > n {
		keep = n
	}
	prefix = messages.Clone(a.historyLocked()[:keep])
	suffix = messages.Clone(a.historyLocked()[keep:])
	usage = a.runtimeUsageLocked()
	sourceBinding = a.modelsState.activeBinding
	rootCtx = a.lifecycle.rootCtx
	metadata = requestJournalMetadata{
		Config:           sourceCfg,
		SettingsRevision: a.sequences.settingsRev,
		ContextRevision:  a.sequences.contextRev,
		CatalogVersion:   a.sequences.catalogVersion,
		Turn:             a.sequences.turnSeq,
		Step:             a.sequences.stepSeq,
		HistoryLen:       len(a.historyLocked()),
	}
	return oldID, sourceCfg, prefix, suffix, usage, sourceBinding, rootCtx, metadata, keep
}

// buildForkBranch assembles the child history: the leading system facts, an
// injected branch-summary system message, and the kept prefix. The tool_call↔
// result boundary is sanitized so the child's first provider request is valid.
func buildForkBranch(prefix []messages.Message, summary, oldID string, branchPoint int, newCreatedAt time.Time) []messages.Message {
	prefix = sanitizeToolPairs(prefix)
	branch := make([]messages.Message, 0, len(prefix)+1)
	insert := 0
	if len(prefix) > 0 && prefix[0].Role == messages.RoleSystem {
		insert = 1
	}
	branch = append(branch, prefix[:insert]...)
	if summary != "" {
		branch = append(branch, messages.Message{
			Role: messages.RoleSystem,
			Content: fmt.Sprintf(
				"This session was branched from session %s before message %d. Work continued from here in a different direction.\n\nSummary of the abandoned direction (already tried — do not repeat unless asked):\n\n%s",
				oldID, branchPoint, summary),
			CreatedAt: newCreatedAt,
		})
	}
	branch = append(branch, prefix[insert:]...)
	for i := range branch {
		messages.EnsureID(&branch[i])
	}
	return branch
}
