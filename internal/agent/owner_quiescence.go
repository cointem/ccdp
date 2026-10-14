package agent

import (
	"ccdp/internal/session"
	"context"
)

type quiescenceRequested struct {
	ctx      context.Context
	closeMCP bool
	result   chan quiescenceResult
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

func (a *Agent) requestQuiescence(ctx context.Context, closeMCP bool) (*sandboxQuiescence, error) {
	if a.owner == nil {
		return a.beginSandboxQuiescence(ctx, closeMCP)
	}
	r := quiescenceRequested{ctx, closeMCP, make(chan quiescenceResult, 1)}
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
	if a.owner == nil {
		a.abortQuiescenceOwned(revision)
		return
	}
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
	resources := a.services.resources
	resume := !uncertain && a.capabilities.capabilityRevision == revision
	if resume {
		a.capabilities.capabilityRevoking = false
		a.capabilities.capabilityRevision++
	}
	a.mu.Unlock()
	if resume && resources != nil && resources.Processes != nil {
		resources.Processes.ResumeAfterRevocation()
	}
}
