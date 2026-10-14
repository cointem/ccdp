package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// goalAccountTarget is the authority delegated to descendant requests. A child
// can read the objective, check its captured budget binding, and transfer a
// completed attempt by durable identity. It cannot mutate the parent's goal or
// reach its config, journal, or locks.
type goalAccountTarget interface {
	SessionID() string
	goalSnapshot() *protocol.Goal
	admitGoalAttempt(id string) bool
	accountGoalFinished(session.RequestManifest, session.Attempt, bool) error
	persistenceFailure() error
}

func (a *Agent) goalSnapshot() *protocol.Goal {
	a.mu.Lock()
	defer a.mu.Unlock()
	return protocol.CloneGoal(a.conversation.durable.Goal)
}
