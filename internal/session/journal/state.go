package journal

import (
	"ccdp/internal/session/replay"
	"sync"
)

// BindState transfers the current projection into the session owner's storage.
// Call only during exclusive construction. Future synchronous commits apply to
// that same state; Writer never maintains a second history/goal/usage projection.
// The read lock protects compatibility readers, and is never held across I/O.
func (p *Writer) BindState(state *replay.State, readLock sync.Locker) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, err := p.projectionLocked()
	if err != nil {
		return err
	}
	readLock.Lock()
	*state = *current
	p.projection = state
	p.stateStorage = state
	p.stateLock = readLock
	readLock.Unlock()
	return nil
}

func (p *Writer) installProjection(state *replay.State) {
	if p.stateStorage == nil {
		p.projection = state
		return
	}
	p.stateLock.Lock()
	*p.stateStorage = *state
	p.projection = p.stateStorage
	p.stateLock.Unlock()
}

// ValidateProjection is the admission barrier after a delivered input. Usually
// this is a cursor comparison; a changed backend must reconcile before work.
func (p *Writer) ValidateProjection() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.projectionLocked()
	return err
}
