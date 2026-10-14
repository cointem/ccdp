package agent

import (
	"context"
	"errors"

	"ccdp/internal/protocol"
	"ccdp/internal/sessionview"
)

type viewsReady struct{}

func (viewsReady) ownerControlMessage() {}

type streamsReady struct{ generation uint64 }

func (streamsReady) ownerControlMessage() {}

type watchResult struct {
	subscription *sessionview.Subscription
	err          error
}
type watchRequested struct {
	ctx    context.Context
	result chan watchResult
}

func (watchRequested) ownerControlMessage() {}

type watchRemoved struct {
	id   uint64
	done chan struct{}
}

func (watchRemoved) ownerControlMessage() {}

type viewsClosed struct{ done chan struct{} }

func (viewsClosed) ownerControlMessage() {}

// Hub state and channels belong exclusively to the session owner. Producers
// freeze updates under mu; the same boundary captures initial watch snapshots.
func (a *Agent) viewHubOwned() *sessionview.Hub {
	if a.views == nil {
		a.views = sessionview.NewHub(a.streamEpoch, a.removeSubscription)
	}
	return a.views
}

func (a *Agent) enqueueViewsLocked(updates ...protocol.Update) {
	if a.owner == nil {
		return
	} // constructor-only test fixtures have no observers
	for _, update := range updates {
		if len(a.publications.pendingViews) >= 256 || a.publications.publicationsLost {
			a.publications.pendingViews = []protocol.Update{{Type: protocol.UpdateResyncRequired, Cursor: update.Cursor, Revision: update.Revision}}
			a.publications.publicationsLost = true
		} else {
			a.publications.pendingViews = append(a.publications.pendingViews, update)
		}
	}
	select {
	case a.owner.control <- viewsReady{}:
	default:
	}
}

func (a *Agent) takeViewsLocked() []protocol.Update {
	a.queueStateOwnedLocked()
	pending := a.publications.pendingViews
	a.publications.pendingViews = nil
	a.publications.publicationsLost = false
	return pending
}
func (a *Agent) publishOwnedViews(updates []protocol.Update) {
	hub := a.viewHubOwned()
	for _, update := range updates {
		hub.Stamp(&update)
		if update.Type == protocol.UpdateResyncRequired {
			hub.Resync(update)
		} else {
			hub.Publish(update)
		}
	}
}
func (a *Agent) drainViews() {
	a.mu.Lock()
	pending := a.takeViewsLocked()
	if len(pending) > 0 {
		a.refreshSnapshotLocked()
	}
	a.mu.Unlock()
	a.publishOwnedViews(pending)
}
func (a *Agent) removeSubscription(id uint64) {
	done := make(chan struct{})
	select {
	case a.owner.control <- watchRemoved{id: id, done: done}:
	case <-a.owner.done:
		return
	}
	select {
	case <-done:
	case <-a.owner.done:
	}
}
func (a *Agent) closeWatchers() {
	done := make(chan struct{})
	select {
	case a.owner.control <- viewsClosed{done: done}:
	case <-a.owner.done:
		return
	}
	select {
	case <-done:
	case <-a.owner.done:
	}
}
func (a *Agent) registerWatch(request watchRequested) {
	a.mu.Lock()
	a.flushStreamsLocked()
	pending := a.takeViewsLocked()
	err := request.ctx.Err()
	if err == nil && (a.lifecycle.closed || a.lifecycle.closing) {
		err = errors.New("agent: session is closed")
	}
	var view protocol.SessionView
	if err == nil {
		a.refreshSnapshotLocked()
		view = protocol.CloneSessionView(*a.publishedSnapshot.Load())
	}
	a.mu.Unlock()
	// Every update captured before the initial snapshot is published first.
	a.publishOwnedViews(pending)
	if err != nil {
		request.result <- watchResult{err: err}
		return
	}
	hub := a.viewHubOwned()
	sub := hub.Subscribe(protocol.Update{Type: protocol.UpdateSnapshot, Cursor: hub.Cursor(view.Revision), Revision: view.Revision, Snapshot: &view})
	request.result <- watchResult{subscription: sub}
	go func() {
		select {
		case <-request.ctx.Done():
			_ = sub.Close()
		case <-a.lifecycle.rootCtx.Done():
			_ = sub.Close()
		case <-sub.Done():
		}
	}()
}
func (a *Agent) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.ensureRun()
	request := watchRequested{ctx: ctx, result: make(chan watchResult, 1)}
	select {
	case a.owner.control <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.owner.done:
		return nil, errors.New("agent: session is closed")
	}
	select {
	case result := <-request.result:
		return result.subscription, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.owner.done:
		return nil, errors.New("agent: session is closed")
	}
}
