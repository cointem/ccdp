package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// requestCompleted contains only the immutable accounting input. Provider
// content and tool calls stay with the worker; their estimate is frozen here.
// This reliable message outlives the request's execution context.
type requestCompleted struct {
	attempt       requestAttempt
	usage         llm.StreamResult
	callErr       error
	goalTokens    int64
	goalEstimated bool
	result        chan requestSettlementResult
}

func (requestCompleted) ownerControlMessage() {}

type requestSettlement struct {
	attempt     session.Attempt
	manifest    session.RequestManifest
	usage       Usage
	applied     bool
	goalChanged bool
}
type requestSettlementResult struct {
	settlement requestSettlement
	err        error
}

func (a *Agent) finishRequestAttempt(attempt requestAttempt, result llm.StreamResult, callErr error) error {
	if a == nil {
		return errors.New("agent: nil agent")
	}
	tokens, estimated := goalAttemptTokens(attempt.estimatedInput, result)
	if llm.RejectedBeforeExecution(callErr) {
		tokens, estimated = 0, false
	}
	completed := requestCompleted{
		attempt: requestAttempt{AttemptID: attempt.AttemptID, RequestID: attempt.RequestID, apiKey: attempt.apiKey, historyLen: attempt.historyLen},
		usage:   llm.StreamResult{PromptTokens: result.PromptTokens, CompletionTok: result.CompletionTok, CachedTokens: result.CachedTokens, CacheReported: result.CacheReported},
		callErr: callErr, goalTokens: tokens, goalEstimated: estimated,
		result: make(chan requestSettlementResult, 1),
	}
	var settled requestSettlement
	var err error
	if a.owner == nil {
		// Unpublished construction fixtures have exclusive ownership.
		settled, err = a.settleRequestCompleted(completed)
	} else {
		a.ensureRun()
		select {
		case a.owner.control <- completed:
		case <-a.owner.done:
			return session.ErrClosed
		}
		select {
		case outcome := <-completed.result:
			settled, err = outcome.settlement, outcome.err
		case <-a.owner.done:
			return session.ErrClosed
		}
	}
	if err != nil {
		return err
	}
	// Cross-session accounting and arbitrary plugin callbacks run outside the
	// owner. A subscriber may synchronously Submit back into this session.
	if err := a.accountDescendantAttempt(settled.attempt); err != nil {
		return err
	}
	if settled.goalChanged {
		a.publishGoal()
	}
	if attempt.goalOwner != nil && attempt.goalOwner != a {
		if err := attempt.goalOwner.accountGoalFinished(settled.manifest, settled.attempt, true); err != nil {
			return err
		}
	}
	if settled.applied && a.services.evbus != nil {
		a.services.evbus.Emit(events.TopicUsageUpdated, events.UsageEvent{
			InputTokens: settled.usage.InputTokens, OutputTokens: settled.usage.OutputTokens,
			TurnCount: settled.usage.TurnCount,
		})
	}
	return nil
}

func (a *Agent) settleRequestCompleted(completed requestCompleted) (requestSettlement, error) {
	attempt, result, callErr := completed.attempt, completed.usage, completed.callErr
	if a == nil {
		return requestSettlement{}, errors.New("agent: nil agent")
	}
	if strings.TrimSpace(attempt.AttemptID) == "" || strings.TrimSpace(attempt.RequestID) == "" {
		return requestSettlement{}, a.poisonRequestJournal(errors.New("agent: request attempt identity is required"))
	}
	if result.PromptTokens < 0 || result.CompletionTok < 0 || result.CachedTokens < 0 {
		return requestSettlement{}, a.poisonRequestJournal(errors.New("agent: provider usage must not be negative"))
	}
	if err := a.persistenceFailure(); err != nil {
		return requestSettlement{}, err
	}
	p := a.persistenceHandle()
	if p == nil {
		err := a.poisonRequestJournal(errors.New("agent: session persistence is unavailable"))
		return requestSettlement{}, err
	}
	manifest, err := p.CommittedRequestManifest(attempt.RequestID)
	if err != nil {
		err = a.poisonRequestJournal(err)
		return requestSettlement{}, err
	}
	if manifest.SessionID != "" && p.Identity() != "" && manifest.SessionID != p.Identity() {
		err := a.poisonRequestJournal(errors.New("agent: prepared request session mismatch"))
		return requestSettlement{}, err
	}
	inputTokens, outputTokens, cachedTokens := result.PromptTokens, result.CompletionTok, result.CachedTokens
	outcome := "success"
	if callErr != nil {
		if isRequestContextAbort(callErr) {
			outcome = "cancelled"
		} else {
			outcome = "error"
		}
	}
	cache := protocol.CacheStats{Tracking: true, InputTokens: inputTokens}
	if result.CacheReported && cachedTokens >= 0 && cachedTokens <= inputTokens {
		cache.CachedTokens = cachedTokens
		cache.ReportedInputTokens = inputTokens
	}
	usage := session.Usage{
		Cache:       cache,
		InputTokens: int64(inputTokens), OutputTokens: int64(outputTokens),
		CachedTokens: int64(cachedTokens), TotalTokens: int64(inputTokens) + int64(outputTokens),
		TurnCount: 1,
	}
	a.mu.Lock()
	currentUsage := a.runtimeUsageLocked()
	a.mu.Unlock()
	goalTokens, goalEstimated := completed.goalTokens, completed.goalEstimated
	finishedAt := time.Now().UTC()
	attemptEvent := session.AttemptFinished{Attempt: session.Attempt{
		GoalTokens: goalTokens, GoalTokensEstimated: goalEstimated,
		AttemptID: attempt.AttemptID, RequestID: attempt.RequestID,
		SessionID: manifest.SessionID, TurnID: manifest.TurnID,
		StepID: manifest.StepID, Purpose: manifest.Purpose,
		Provider: manifest.Provider, Model: manifest.Model,
		Endpoint: manifest.Endpoint, Outcome: outcome,
		Error: safeAttemptError(callErr, attempt.apiKey, manifest.Endpoint),
		Usage: usage, FinishedAt: finishedAt,
	}}
	if prior, ok := p.Writer.CompletedAttempt(attempt.RequestID); ok {
		retry := attemptEvent.Attempt
		retry.FinishedAt = prior.FinishedAt
		if retry != prior {
			return requestSettlement{}, a.poisonRequestJournal(errors.New("agent: request already settled with different data"))
		}
		return requestSettlement{attempt: prior, manifest: manifest, usage: currentUsage}, nil
	}
	candidate, err := addRuntimeUsage(currentUsage, usage, true)
	if err != nil {
		return requestSettlement{}, a.poisonRequestJournal(err)
	}
	absolute := session.Usage{
		InputTokens: int64(candidate.InputTokens), OutputTokens: int64(candidate.OutputTokens),
		CachedTokens: int64(candidate.CachedTokens), Cache: candidate.Cache, TotalTokens: int64(candidate.InputTokens + candidate.OutputTokens),
		TurnCount: int64(candidate.TurnCount),
	}
	revision := uint64(p.CurrentCursor() + 1)
	facts := []session.Event{attemptEvent, session.UsageChanged{Revision: revision, Usage: absolute}}
	var goalFact *session.GoalChanged
	if manifest.GoalSessionID == a.conversation.sessionID && manifest.GoalID != "" {
		goalFact, err = a.prepareGoalAccountingLocked(manifest, attemptEvent.Attempt)
		if err != nil {
			return requestSettlement{}, a.poisonRequestJournal(err)
		}
		if goalFact != nil {
			facts = append(facts, *goalFact)
		}
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: "attempt-finished-" + attempt.AttemptID,
		Events:        facts,
	}); err != nil {
		err = a.poisonRequestJournal(fmt.Errorf("agent: persist AttemptFinished: %w", err))
		return requestSettlement{}, err
	}
	if goalFact != nil {
		a.applyGoalAccounting(*goalFact)
	}
	a.mu.Lock()
	// Main requests establish the same prompt-token baseline the old usage
	// path established. Guardian/sub-agent callers can retain their separate
	// history baseline by using their own purpose.
	if callErr == nil && (manifest.Purpose == "main" || manifest.Purpose == "chat" || manifest.Purpose == "completion" || manifest.Purpose == "task" || manifest.Purpose == "guardian") {
		a.turnMemory.tokenBaseline.promptTokens = inputTokens
		a.turnMemory.tokenBaseline.historyLen = attempt.historyLen
	}
	a.mu.Unlock()
	// Publish the committed projection before returning to the worker, which
	// performs cross-session transfers and invokes arbitrary plugin callbacks.
	a.emit(Event{Type: EventUsage, Usage: &candidate})
	return requestSettlement{applied: true, attempt: attemptEvent.Attempt, manifest: manifest, usage: candidate, goalChanged: goalFact != nil}, nil
}
