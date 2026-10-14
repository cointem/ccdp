package agent

import (
	"context"
	"sync"
	"testing"

	"ccdp/internal/protocol"
)

var testWatchStreams sync.Map

// Tests consume the same typed subscription as the clients.
func watchTestEvents(t *testing.T, a *Agent) <-chan protocol.EventView {
	t.Helper()
	if ch, ok := testWatchStreams.Load(a); ok {
		return ch.(chan protocol.EventView)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := a.Watch(ctx, protocol.Cursor{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ch := make(chan protocol.EventView, 4096)
	testWatchStreams.Store(a, ch)
	t.Cleanup(func() { cancel(); _ = sub.Close(); testWatchStreams.Delete(a) })
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-sub.Updates():
				if !ok {
					return
				}
				if update.Event != nil {
					select {
					case ch <- *update.Event:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch
}
