package agent

import (
	"errors"
	"fmt"

	"ccdp/internal/protocol"
)

// persistGoal commits the goal before callers publish the new runtime state.
func (a *Agent) persistGoal(goal *protocol.Goal) error {
	return a.persistGoalLocked(goal)
}

// persistGoalLocked requires session ownership; it does not mutate a.goal.
func (a *Agent) persistGoalLocked(goal *protocol.Goal) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := p.PersistGoal(goal); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist goal: %w", err))
	}
	return nil
}

func cloneGoalAccountingIDs(ids map[string]bool) map[string]bool {
	if ids == nil {
		return nil
	}
	copy := make(map[string]bool, len(ids))
	for id, accounted := range ids {
		if accounted {
			copy[id] = true
		}
	}
	return copy
}
