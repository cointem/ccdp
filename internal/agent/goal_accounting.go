package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// Provider usage is authoritative when present. Persist estimates alongside
// the attempt, so recovery never needs to reconstruct a partial response.
func goalAttemptTokens(estimatedInput int, result llm.StreamResult) (int64, bool) {
	inputReported := result.PromptReported || result.PromptTokens > 0
	outputReported := result.CompletionReported || result.CompletionTok > 0
	input := int64(max(0, result.PromptTokens-result.CachedTokens))
	if !inputReported {
		input = int64(max(0, estimatedInput))
	}
	output := int64(max(0, result.CompletionTok))
	if !outputReported {
		encoded, _ := json.Marshal(struct {
			Text      string         `json:"text,omitempty"`
			Reasoning string         `json:"reasoning,omitempty"`
			Tools     []llm.ToolCall `json:"tool_calls,omitempty"`
		}{result.Text, result.Reasoning, result.ToolCalls})
		output = int64(llm.EstimateTokens(string(encoded)))
	}
	return input + output, !inputReported || !outputReported
}

// The caller is the session owner. For an owner request the returned event joins
// AttemptFinished in the same transaction, closing the crash window between
// recording provider usage and charging the goal.
func (a *Agent) prepareGoalAccountingLocked(manifest session.RequestManifest, attempt session.Attempt) (*session.GoalChanged, error) {
	if manifest.GoalID == "" {
		return nil, nil
	}
	a.mu.Lock()
	goal := protocol.CloneGoal(a.conversation.durable.Goal)
	sessionID := a.conversation.sessionID
	key := manifest.SessionID + ":" + attempt.AttemptID
	counted := a.conversation.durable.GoalAccountingIDs[key]
	a.mu.Unlock()
	if manifest.GoalSessionID != sessionID || goal == nil || goal.ID != manifest.GoalID || counted {
		return nil, nil
	}
	if manifest.SessionID == "" || attempt.AttemptID == "" || attempt.RequestID != manifest.RequestID {
		return nil, errors.New("goal accounting request identity mismatch")
	}
	if attempt.GoalTokens < 0 || goal.TokensUsed > math.MaxInt64-attempt.GoalTokens || goal.ModelCalls == math.MaxInt || goal.RunModelCalls == math.MaxInt {
		return nil, errors.New("goal accounting counter overflow")
	}
	goal.TokensUsed += attempt.GoalTokens
	goal.ModelCalls++
	goal.RunModelCalls++
	goal.TokensEstimated = goal.TokensEstimated || attempt.GoalTokensEstimated
	goal.UpdatedAt = time.Now().UTC()
	if goal.Status == protocol.GoalActive && goal.TokenBudget > 0 && goal.TokensUsed >= goal.TokenBudget {
		goal.Status = protocol.GoalBudgetLimited
		goal.Reason = "goal token budget reached"
	}
	return &session.GoalChanged{Goal: goal, AccountingID: key}, nil
}

// Called only after the accounting event commits, on the session owner.
func (a *Agent) applyGoalAccounting(fact session.GoalChanged) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conversation.goalTurnCalls++
}

// Child attempts have their durable completion in another session log. The
// request's captured owner binding plus the stable accounting id lets startup
// complete a missed transfer without charging a request twice.
func (a *Agent) accountGoalFinished(manifest session.RequestManifest, attempt session.Attempt, publish bool) error {
	if publish {
		return a.settleGoalAttempt(manifest, attempt)
	}
	return a.accountGoalFinishedOwned(manifest, attempt, false)
}

func (a *Agent) accountGoalFinishedOwned(manifest session.RequestManifest, attempt session.Attempt, publish bool) error {
	fact, err := a.prepareGoalAccountingLocked(manifest, attempt)
	if err == nil && fact != nil {
		p := a.persistenceHandle()
		if p == nil {
			err = errors.New("goal persistence unavailable")
		} else {
			_, err = p.Commit(session.Batch{TransactionID: "goal-attempt-" + fact.AccountingID, Events: []session.Event{*fact}})
		}
		if err == nil {
			a.applyGoalAccounting(*fact)
		}
	}
	if err != nil {
		a.markPersistenceFailure(err)
		return err
	}
	if fact != nil && publish {
		a.publishGoal()
	}
	return nil
}

// recoverGoalAccounting reads only children assigned to this specific goal.
// A damaged child blocks the goal but does not make the parent session
// unavailable; only failures of the parent journal abort construction.
func (a *Agent) recoverGoalAccounting() error {
	_, err := a.recoverGoalAccountingState()
	return err
}

// The ready result distinguishes unreadable child accounting from a failure
// of the parent journal. Construction tolerates the former; resume cannot.
func (a *Agent) recoverGoalAccountingState() (ready bool, err error) {
	a.mu.Lock()
	goalID := ""
	if a.conversation.durable.Goal != nil {
		goalID = a.conversation.durable.Goal.ID
	}
	rootID := a.conversation.sessionID
	dir := a.services.cfg.SessionDir
	memory := a.services.cfg.NoSessionPersistence
	a.mu.Unlock()
	if goalID == "" {
		return true, nil
	}
	p := a.persistenceHandle()
	if p == nil {
		return false, errors.New("goal recovery persistence unavailable")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return false, err
	}
	visited := map[string]bool{}
	var visit func(string, []session.Record) error
	visit = func(id string, records []session.Record) error {
		if visited[id] {
			return nil
		}
		visited[id] = true
		childIDs := make(map[string]bool)
		for _, record := range records {
			switch fact := record.Event.(type) {
			case *session.ChildRunRecorded:
				if string(fact.Child.ParentSessionID) == id && fact.Child.SessionID != "" && fact.GoalSessionID == rootID && fact.GoalID == goalID {
					childIDs[string(fact.Child.SessionID)] = true
				}
			}
		}
		entries, err := a.requestAccountingAttempts(id, records, func(manifest session.RequestManifest) bool {
			return manifest.GoalSessionID == rootID && manifest.GoalID == goalID
		})
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.recovered && id == rootID {
				if err := a.accountRecoveredOwnAttempt(entry); err != nil {
					return err
				}
			}
			if entry.recovered && id != rootID {
				if err := a.accountSharedAttempt(entry.attempt); err != nil {
					return err
				}
			}
			if err := a.accountGoalFinished(entry.manifest, entry.attempt, false); err != nil {
				return err
			}
		}
		if memory {
			return nil
		}
		for childID := range childIDs {
			if visited[childID] {
				continue
			}
			store, err := session.OpenJSONLReadOnly(dir, childID)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("goal recovery child %s: %w", childID, err)
			}
			childRecords, readErr := store.Read(session.Beginning)
			closeErr := store.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return fmt.Errorf("goal recovery child %s: %w", childID, err)
			}
			if err := visit(childID, childRecords); err != nil {
				return err
			}
		}
		return nil
	}
	err = visit(rootID, records)
	if err == nil {
		return true, nil
	}
	if a.persistenceFailure() != nil {
		return false, err
	}
	_, persistErr := a.changeGoal(func(goal *protocol.Goal) (*protocol.Goal, error) {
		if goal != nil && goal.ID == goalID {
			goal.Status = protocol.GoalBlocked
			goal.Reason = "Goal accounting recovery incomplete: " + err.Error()
		}
		return goal, nil
	})
	return false, persistErr
}

func bindChildGoal(fact *session.ChildRunRecorded, opts Options) {
	fact.GoalSessionID, fact.GoalID = "", ""
	if opts.goalOwner != nil && opts.goalOwnerID != "" {
		fact.GoalSessionID, fact.GoalID = opts.goalOwner.SessionID(), opts.goalOwnerID
	}
}
