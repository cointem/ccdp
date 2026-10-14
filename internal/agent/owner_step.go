package agent

import "ccdp/internal/session"

type stepRequested struct{ result chan stepResult }

func (stepRequested) ownerControlMessage() {}

type stepResult struct {
	step stepRuntime
	err  error
}
type stepReleased struct {
	turn, step uint64
	done       chan struct{}
}

func (stepReleased) ownerControlMessage() {}

func (a *Agent) requestStepRuntime() (stepRuntime, error) {
	if a.owner == nil {
		return a.captureStepOwned()
	}
	a.ensureRun()
	request := stepRequested{result: make(chan stepResult, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return stepRuntime{}, session.ErrClosed
	}
	// The caller always receives an admitted lease, including on cancellation,
	// and releases it through its normal step cleanup. No abandoned lease reply.
	select {
	case result := <-request.result:
		return result.step, result.err
	case <-a.owner.done:
		// A completed owner may already have transferred a lease to this
		// buffered reply. The receiver still owns releasing that lease.
		select {
		case result := <-request.result:
			return result.step, result.err
		default:
			return stepRuntime{}, session.ErrClosed
		}
	}
}

func (a *Agent) clearStepContext(turn, step uint64) {
	a.mu.Lock()
	if a.children.childStep != nil && a.children.childStep.turn == turn && a.children.childStep.step == step {
		a.children.childStep = nil
	}
	a.mu.Unlock()
}
