package agent

import (
	"errors"

	"ccdp/internal/session/archive"
)

// OpenSession prepares an independent Agent handle for a saved session. It
// never mutates this Agent: the caller can atomically replace its UI handle
// only after OpenSession succeeds, then close the old handle.
func (a *Agent) OpenSession(id string) (*Agent, error) {
	if a == nil || a.services.cfg == nil {
		return nil, errors.New("agent: cannot open a session from a nil agent")
	}
	a.mu.Lock()
	busy, closing := a.turn.busy, a.lifecycle.closing
	// Clone all mutable config state while holding the Agent lock. The opened
	// handle must not share provider maps, policy slices, or session channels
	// with the source; a late command on the old handle must never be consumed
	// by the new session.
	cfg := cloneConfig(&a.services.baseCfg)
	a.mu.Unlock()
	if busy {
		return nil, errors.New("agent: cannot open a session while a turn is running")
	}
	if closing {
		return nil, errors.New("agent: cannot open a session while closing")
	}
	var snapshot *archive.Snapshot
	var opts Options
	var err error
	if cfg.NoSessionPersistence {
		catalog := a.memorySessionCatalog()
		if err = a.retainMemorySession(catalog); err != nil {
			return nil, err
		}
		catalog.mu.Lock()
		if saved := catalog.sessions[id]; saved != nil {
			snapshot = saved.snapshot
			opts.memoryCoding, opts.memoryChildren = saved.coding, saved.children
		}
		catalog.mu.Unlock()
	}
	if snapshot == nil {
		snapshot, err = archive.Load(cfg.SessionDir, id)
	}
	if err != nil {
		return nil, err
	}
	opened, err := resumeWithOptions(&cfg, snapshot, opts)
	if err == nil && cfg.NoSessionPersistence {
		opened.memorySessions = a.memorySessionCatalog()
	}
	return opened, err
}
