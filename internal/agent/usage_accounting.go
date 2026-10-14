package agent

import (
	"errors"
	"fmt"
	"os"

	"ccdp/internal/session"
)

type usageAccountTarget interface{ settleSharedAttempt(session.Attempt) error }

func (a *Agent) usageOwner() usageAccountTarget {
	if a.services.supervisor != nil && a.services.supervisor.root != nil {
		return a.services.supervisor.root
	}
	return a
}

// Reconcile finished and abandoned prepared requests throughout the tree
// when opening the session. Unknown outcomes retain their entire
// frozen reservation as estimated usage.
func (a *Agent) reconcileDescendantUsage() error {
	p := a.persistenceHandle()
	if p == nil {
		return nil
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	entries, err := a.requestAccountingAttempts(a.conversation.sessionID, records, nil)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.recovered {
			if err := a.accountRecoveredOwnAttempt(entry); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{a.conversation.sessionID: true}
	var visit func([]session.Record) error
	visit = func(records []session.Record) error {
		for _, record := range records {
			child, ok := record.Event.(*session.ChildRunRecorded)
			if !ok {
				continue
			}
			id := string(child.Child.SessionID)
			if seen[id] {
				continue
			}
			seen[id] = true
			store, err := session.OpenJSONLReadOnly(a.services.cfg.SessionDir, id)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			rows, err := store.Read(session.Beginning)
			_ = store.Close()
			if err != nil {
				return err
			}
			entries, err := a.requestAccountingAttempts(id, rows, nil)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := a.accountSharedAttempt(entry.attempt); err != nil {
					return err
				}
			}
			if err := visit(rows); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(records)
}

// Descendant attempts are copied to the root immediately. Existing attempt
// identities and UsageChanged projection provide durable idempotence.
func (a *Agent) accountDescendantAttempt(attempt session.Attempt) error {
	root := a.usageOwner()
	if root == a {
		return nil
	}
	return root.settleSharedAttempt(attempt)
}

func (a *Agent) accountSharedAttempt(attempt session.Attempt) error {
	p := a.persistenceHandle()
	if p == nil {
		return fmt.Errorf("root usage writer unavailable")
	}
	if p.SharedUsageAccounted(attempt.SessionID, attempt.AttemptID) {
		return nil
	}
	a.mu.Lock()
	u := a.runtimeUsageLocked()
	a.mu.Unlock()
	u, err := addRuntimeUsage(u, attempt.Usage, false)
	if err != nil {
		return a.poisonRequestJournal(err)
	}
	// CacheStats describes this session's own prompt reuse. Aggregate token
	// counters above include descendants; their cache ratios stay in their views.
	_, err = p.CommitEvents("shared-attempt-"+attempt.AttemptID, session.UsageChanged{SourceSessionID: attempt.SessionID, SourceAttemptID: attempt.AttemptID, Usage: session.Usage{
		InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CachedTokens: int64(u.CachedTokens), Cache: u.Cache,
		TotalTokens: int64(u.InputTokens + u.OutputTokens), TurnCount: int64(u.TurnCount),
	}})
	if err != nil {
		return a.poisonRequestJournal(err)
	}
	a.mu.Lock()
	a.mu.Unlock()
	a.emit(Event{Type: EventUsage, Usage: &u})
	if attempt.UsageEstimated && attempt.Outcome == "unknown" {
		a.emitStatus("recovered request %s in child %s; token usage estimated conservatively for %d input and %d output tokens", attempt.RequestID, attempt.SessionID, attempt.Usage.InputTokens, attempt.Usage.OutputTokens)
	}
	return nil
}
