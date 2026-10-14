package main

import (
	"ccdp/internal/protocol"
	"context"
	"testing"
	"time"
)

func statePointer(v protocol.SessionView) *protocol.SessionState { s := v.State(); return &s }

type delayedWatchClient struct {
	headlessTestClient
	release chan struct{}
	late    *headlessTestSubscription
}

func (c *delayedWatchClient) Watch(context.Context, protocol.Cursor) (protocol.Subscription, error) {
	<-c.release
	return c.late, nil
}

func TestHeadlessWatchDeadlineDoesNotJoinUnresponsiveConstructor(t *testing.T) {
	client := &delayedWatchClient{release: make(chan struct{}), late: &headlessTestSubscription{ch: make(chan protocol.Update, 1)}}
	watch := &headlessWatch{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- watch.openWithDeadline(context.Background(), ctx) }()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("expected startup timeout")
		}
	case <-time.After(300 * time.Millisecond):
		close(client.release)
		t.Fatal("timeout blocked joining Watch")
	}
	close(client.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.late.mu.Lock()
		closed := client.late.closed
		client.late.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("late subscription was not closed")
}
