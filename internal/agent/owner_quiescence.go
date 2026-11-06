package agent

import (
	"ccdp/internal/session"
	"context"
)

type quiescenceRequested struct {
	ctx          context.Context
	closeMCP     bool
	allowPending bool
	result       chan quiescenceResult
}
type quiescenceResult struct {
	scope *sandboxQuiescence
	err   error
}

func (quiescenceRequested) ownerControlMessage() {}

type quiescenceAborted struct {
	revision uint64
	done     chan struct{}
}

func (quiescenceAborted) ownerControlMessage() {}

func (a *Agent) requestQuiescence(ctx context.Context, closeMCP bool, allowPending ...bool) (*sandboxQuiescence, error) {
	allow := len(allowPending) > 0 && allowPending[0]

	r := quiescenceRequested{ctx: ctx, closeMCP: closeMCP, allowPending: allow, result: make(chan quiescenceResult, 1)}
	a.ensureRun()
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return nil, session.ErrClosed
	}
	select {
	case out := <-r.result:
		return out.scope, out.err
	case <-a.owner.done:
		return nil, session.ErrClosed
	}
}
func (a *Agent) abortQuiescence(revision uint64) {
	r := quiescenceAborted{revision, make(chan struct{})}
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
func (a *Agent) abortQuiescenceOwned(revision uint64) {
	a.mu.Lock()
	uncertain := a.services.sandbox != nil && a.services.sandbox.ExternalExecutionPossible()
	resume := !uncertain && a.capabilities.capabilityRevision == revision
	if resume {
		a.capabilities.capabilityRevision++
		a.capabilities.revocationRevision++
	}
	a.mu.Unlock()
	if resume {
		_ = a.resumeSandboxAdmission()
	}
}
