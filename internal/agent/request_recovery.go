package agent

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type accountingAttempt struct {
	manifest  session.RequestManifest
	attempt   session.Attempt
	recovered bool
}

// RequestPrepared and AttemptFinished are the accounting lifecycle for both
// token accounting and goal budgets. Recovery must include requests whose provider outcome
// was never committed, using the upper bound captured before execution.
func (a *Agent) requestAccountingAttempts(id string, records []session.Record, include func(session.RequestManifest) bool) ([]accountingAttempt, error) {
	manifests := map[string]session.RequestManifest{}
	finished := map[string]bool{}
	var order []string
	for _, record := range records {
		switch fact := session.EventPointer(record.Event).(type) {
		case *session.RequestPrepared:
			if fact.Manifest.SessionID == id {
				if _, exists := manifests[fact.Manifest.RequestID]; !exists {
					order = append(order, fact.Manifest.RequestID)
				}
				manifests[fact.Manifest.RequestID] = fact.Manifest
			}
		case *session.AttemptFinished:
			finished[fact.Attempt.RequestID] = true
		}
	}
	var attempts []accountingAttempt
	for _, record := range records {
		if fact, ok := session.EventPointer(record.Event).(*session.AttemptFinished); ok {
			manifest := manifests[fact.Attempt.RequestID]
			if include == nil || include(manifest) {
				attempts = append(attempts, accountingAttempt{manifest: manifest, attempt: fact.Attempt})
			}
		}
	}
	for _, requestID := range order {
		manifest := manifests[requestID]
		if finished[requestID] || include != nil && !include(manifest) || a.requestAdmittedInProcess(id, requestID) {
			continue
		}
		if manifest.ReservedUsage == nil {
			return nil, fmt.Errorf("request %s in session %s has unknown usage and no durable settlement bound", requestID, id)
		}
		usage := *manifest.ReservedUsage
		usage.Cache = protocol.CacheStats{Tracking: true, InputTokens: int(usage.InputTokens)}
		usage.TurnCount = 1
		attempts = append(attempts, accountingAttempt{manifest: manifest, recovered: true, attempt: session.Attempt{
			AttemptID: messages.StableID("recovered", []string{id, requestID}), RequestID: requestID,
			SessionID: id, TurnID: manifest.TurnID, StepID: manifest.StepID, Purpose: manifest.Purpose,
			Provider: manifest.Provider, Model: manifest.Model, Endpoint: manifest.Endpoint,
			Outcome: "unknown", Error: "process ended before provider usage was committed; settled at the reserved upper bound",
			Usage: usage, UsageEstimated: true, GoalTokens: usage.TotalTokens, GoalTokensEstimated: true,
		}})
	}
	return attempts, nil
}

// Only the current writer knows which requests belong to this process. A
// child's log can be read during a live goal resume, so consult its writer too.
func (a *Agent) requestAdmittedInProcess(sessionID, requestID string) bool {
	var owner interface{ RequestAdmitted(string) bool } = a
	if sessionID != a.conversation.sessionID {
		if a.services.supervisor == nil {
			return false
		}
		run := a.services.supervisor.directory.Current(protocol.SessionID(sessionID))
		if run == nil {
			return false
		}
		run.mu.Lock()
		owner = run.agent
		run.mu.Unlock()
		if owner == nil {
			return false
		}
	}
	return owner.RequestAdmitted(requestID)
}

// Root-local unknown attempts commit their completion, usage and goal charge
// together. Descendants transfer the same stable recovery ID through existing
// shared usage/goal accounting and can commit it locally if later reopened.
func (a *Agent) accountRecoveredOwnAttempt(entry accountingAttempt) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("request recovery persistence unavailable")
	}
	if p.RequestFinished(entry.attempt.RequestID) || p.RequestAdmitted(entry.attempt.RequestID) {
		return nil
	}
	a.mu.Lock()
	u := a.runtimeUsageLocked()
	a.mu.Unlock()
	charge := entry.attempt.Usage
	charge.TurnCount = 1
	u, err := addRuntimeUsage(u, charge, true)
	if err != nil {
		return a.poisonRequestJournal(err)
	}
	entry.attempt.FinishedAt = time.Now().UTC()
	goalFact, err := a.prepareGoalAccountingLocked(entry.manifest, entry.attempt)
	if err != nil {
		return a.poisonRequestJournal(err)
	}
	facts := []session.Event{session.AttemptFinished{Attempt: entry.attempt}, session.UsageChanged{Usage: session.Usage{
		InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CachedTokens: int64(u.CachedTokens),
		TotalTokens: int64(u.InputTokens) + int64(u.OutputTokens), Cache: u.Cache, TurnCount: int64(u.TurnCount),
	}}}
	if goalFact != nil {
		facts = append(facts, *goalFact)
	}
	if _, err := p.CommitEvents("recover-request-"+entry.attempt.RequestID, facts...); err != nil {
		return a.poisonRequestJournal(err)
	}
	if goalFact != nil {
		a.applyGoalAccounting(*goalFact)
	}
	a.mu.Lock()
	a.mu.Unlock()
	a.emit(Event{Type: EventUsage, Usage: &u})
	a.emitStatus("recovered request %s; token usage estimated conservatively for %d input and %d output tokens", entry.attempt.RequestID, charge.InputTokens, charge.OutputTokens)
	return nil
}
