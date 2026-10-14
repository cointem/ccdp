package agent

import "ccdp/internal/protocol"

type closeStarted struct{ done chan struct{} }

func (closeStarted) ownerControlMessage() {}

type writerCloseRequested struct{ result chan error }

func (writerCloseRequested) ownerControlMessage() {}

type closeFinished struct {
	err  error
	done chan struct{}
}

func (closeFinished) ownerControlMessage() {}

func (a *Agent) beginCloseOwned() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lifecycle.closing = true
	if a.finalization != nil && !a.turn.busy {
		a.lifecycle.phase = protocol.PhaseFinalizing
	} else {
		a.lifecycle.phase = protocol.PhaseStopping
	}
	a.turn.interruptFlag = true
	if a.turn.turnCancel != nil {
		a.turn.turnCancel()
	}
}

func (a *Agent) finishCloseOwned(err error) {
	a.mu.Lock()
	a.lifecycle.closeErr = err
	a.lifecycle.closed = true
	a.lifecycle.phase = protocol.PhaseClosed
	a.mu.Unlock()
	a.publishState()
	a.drainViews()
}
