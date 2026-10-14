package agent

import (
	"ccdp/internal/session"
	"fmt"
)

// Prepared bookkeeping facts never grant model/tool execution. These distinct
// domain events may be prepared by workspace workers; their commit remains on
// the session owner, including after cancellation while workers are joining.
type auxiliaryFactPrepared struct {
	transactionID string
	fact          session.Event
	result        chan auxiliaryCommitResult
}
type auxiliaryCommitResult struct {
	commit session.CommitResult
	err    error
}

func (auxiliaryFactPrepared) ownerControlMessage() {}

func (a *Agent) commitAuxiliaryOwned(r auxiliaryFactPrepared) auxiliaryCommitResult {
	p := a.persistenceHandle()
	if p == nil {
		return auxiliaryCommitResult{err: session.ErrClosed}
	}
	result, err := p.CommitEvents(r.transactionID, r.fact)
	return auxiliaryCommitResult{result, err}
}
func (a *Agent) commitAuxiliary(transactionID string, fact session.Event) (session.CommitResult, error) {
	switch fact.(type) {
	case session.RecoveryPointRecorded, session.FileMutationRecorded, session.RestoreRecorded, session.ReviewRecorded, session.DeliveryRecorded:
	default:
		return session.CommitResult{}, fmt.Errorf("unsupported workspace settlement %T", fact)
	}
	// Event codec freezes slices/maps before crossing the owner boundary.
	frozen, err := session.CloneEvent(fact)
	if err != nil {
		return session.CommitResult{}, err
	}
	r := auxiliaryFactPrepared{transactionID: transactionID, fact: frozen, result: make(chan auxiliaryCommitResult, 1)}
	if a.owner == nil {
		out := a.commitAuxiliaryOwned(r)
		return out.commit, out.err
	}
	a.ensureRun()
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return session.CommitResult{}, session.ErrClosed
	}
	select {
	case out := <-r.result:
		return out.commit, out.err
	case <-a.owner.done:
		return session.CommitResult{}, session.ErrClosed
	}
}

type memoryClearRequested struct{ result chan error }

func (memoryClearRequested) ownerControlMessage() {}
func (a *Agent) clearMemoryOwned() error {
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	if err := p.PersistMemory("", true); err != nil {
		a.markPersistenceFailure(err)
		return err
	}
	return nil
}

type directoryAccessRequested struct {
	dir    string
	denied bool
	done   chan struct{}
}

func (directoryAccessRequested) ownerControlMessage() {}
func (a *Agent) setDirectoryAccessOwned(r directoryAccessRequested) {
	a.mu.Lock()
	if a.services.sandbox != nil {
		if r.denied {
			a.services.sandbox.AddDisallowedDir(r.dir)
		} else {
			a.services.sandbox.AddDir(r.dir)
		}
	}
	a.mu.Unlock()
	if r.denied {
		a.emitStatus("disallowed directory: %s", r.dir)
	} else {
		a.emitStatus("additional directory: %s", r.dir)
	}
}
func (a *Agent) setDirectoryAccess(dir string, denied bool) {
	r := directoryAccessRequested{dir: dir, denied: denied, done: make(chan struct{})}
	if a.owner == nil {
		a.setDirectoryAccessOwned(r)
		return
	}
	a.ensureRun()
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return
	}
	select {
	case <-r.done:
	case <-a.owner.done:
	}
}

type turnStartRequested struct {
	turnID, stepID, inputID string
	result                  chan error
}

func (turnStartRequested) ownerControlMessage() {}
func (a *Agent) startTurnJournal(turnID, stepID, inputID string) error {
	if a.owner == nil {
		return a.persistTurnStarted(turnID, stepID, inputID)
	}
	r := turnStartRequested{turnID, stepID, inputID, make(chan error, 1)}
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-r.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
