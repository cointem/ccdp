package agent

import (
	"context"
	"fmt"

	"ccdp/internal/session"
)

// Artifact preparation runs outside the state loop. Admission receives the
// frozen manifest only after its blob has been written and verified.
type requestPrepared struct {
	ctx      context.Context
	manifest session.RequestManifest
	result   chan error
}

func (requestPrepared) ownerControlMessage() {}

func (a *Agent) admitPreparedRequest(ctx context.Context, manifest session.RequestManifest) error {
	request := requestPrepared{ctx: ctx, manifest: manifest, result: make(chan error, 1)}
	if a.owner == nil {
		return a.commitPreparedRequest(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-a.owner.done:
		return session.ErrClosed
	}
	// Once queued, consume the admission result even if the execution context
	// expires. An accepted RequestPrepared must not be mistaken for no admission.
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

func (a *Agent) commitPreparedRequest(request requestPrepared) error {
	if err := request.ctx.Err(); err != nil {
		return err
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	a.mu.Lock()
	closing := a.lifecycle.closing || a.lifecycle.closed
	a.mu.Unlock()
	if closing {
		return context.Canceled
	}
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	if _, err := p.Commit(session.Batch{TransactionID: "request-prepared-" + request.manifest.RequestID, Events: []session.Event{session.RequestPrepared{Manifest: request.manifest}}}); err != nil {
		return fmt.Errorf("agent: persist RequestPrepared: %w", err)
	}
	return nil
}
