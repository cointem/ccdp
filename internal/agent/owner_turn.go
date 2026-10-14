package agent

import (
	"ccdp/internal/protocol"
)

func (a *Agent) isBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn.busy || a.lifecycle.settling
}

func (a *Agent) setPhase(phase protocol.RuntimePhase) {
	if a.owner == nil {
		a.setPhaseOwned(phase)
		return
	}
	a.ensureRun()
	request := turnPhaseChanged{phase: phase, done: make(chan struct{})}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return
	}
	select {
	case <-request.done:
	case <-a.owner.done:
	}
}

func (a *Agent) setPhaseOwned(phase protocol.RuntimePhase) {
	a.mu.Lock()
	if !a.lifecycle.closed {
		a.lifecycle.phase = phase
	}
	a.mu.Unlock()
	a.publishState()
}

func (a *Agent) stopTurn() {
	a.cancelWork("stopped by user")
	a.mu.Lock()
	a.lifecycle.phase = protocol.PhaseStopping
	a.mu.Unlock()
	a.emitStatus("stop requested")
	a.publishState()
}
