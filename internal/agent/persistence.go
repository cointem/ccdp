package agent

import (
	"errors"

	"ccdp/internal/session/journal"
)

func (a *Agent) openPersistence() error {
	return a.openPersistenceMode(false)
}

func (a *Agent) openPersistenceFresh() error {
	return a.openPersistenceMode(true)
}

func (a *Agent) openPersistenceMode(rejectExisting bool, backend ...journal.BackendOptions) error {
	if a == nil || a.services.cfg == nil {
		return errors.New("agent: persistence requires an initialized agent")
	}
	a.mu.Lock()
	parentID := a.conversation.parentID
	source := "agent"
	if a.children.childState != nil {
		source = a.children.childState.purpose
	}
	a.mu.Unlock()
	p, err := openSessionPersistenceModeWithParent(a.services.cfg, a.conversation.sessionID, a.conversation.createdAt, source, rejectExisting, parentID, backend...)
	if err != nil {
		a.mu.Lock()
		a.services.persistenceErr = err
		a.mu.Unlock()
		return err
	}
	if err := p.Writer.BindState(&a.conversation.durable, &a.mu); err != nil {
		_ = p.Close()
		return err
	}
	if _, err := usageFromSession(a.conversation.durable.Usage); err != nil {
		_ = p.Close()
		return err
	}
	a.mu.Lock()
	a.services.persistence = p
	a.transcript = p.TranscriptView()
	a.services.persistenceErr = nil
	a.mu.Unlock()
	return nil
}

func (a *Agent) closePersistence() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	p := a.services.persistence
	if p != nil {
		// Once the store is detached, revisionLocked falls back to logSeq.
		// Keep the durable cursor so the final idle/closed snapshot is not
		// rejected by observers as older than the last running snapshot.
		a.sequences.logSeq = max(a.sequences.logSeq, uint64(p.CurrentCursor()))
	}
	a.services.persistence = nil
	a.mu.Unlock()
	if p == nil {
		return nil
	}
	err := p.Close()
	if err != nil {
		a.mu.Lock()
		a.services.persistenceErr = err
		a.mu.Unlock()
	}
	return err
}

// persistSessionClosed records the lifecycle terminal fact before the store is
// released. Close is already past all turn/operation joins here, so this fact
// cannot race a producer and a replay can distinguish a clean close from a
// crash after SessionCreated.
func (a *Agent) persistSessionClosed(outcome string) error {
	if a == nil {
		return nil
	}
	if outcome == "" {
		outcome = "success"
	}
	p := a.persistenceHandle()
	if p == nil {
		return nil
	}
	return p.PersistClosed(outcome)
}

func (a *Agent) persistenceHandle() *sessionPersistence {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.persistence
}

// persistenceFailure reports a poisoned durable writer. Runtime admission
// uses this gate to stop accepting work after a synced commit failed; callers
// must not continue to start a turn against an uncertain log.
func (a *Agent) persistenceFailure() error {
	if a != nil {
		if fault := a.persistenceFault.Load(); fault != nil {
			return *fault
		}
	}
	if a == nil {
		return errors.New("agent: nil agent")
	}
	a.mu.Lock()
	err := a.services.persistenceErr
	p := a.services.persistence
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if p != nil {
		return p.Failure()
	}
	return nil
}

// sessionPersistence adapts Agent domain values to its private journal writer.
type sessionPersistence struct{ *journal.Writer }
