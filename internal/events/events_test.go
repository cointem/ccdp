package events

import (
	"sync"
	"testing"
)

func TestSubscribeDisposeOrderIndependent(t *testing.T) {
	b := NewBus()
	var got []string
	d1 := b.Subscribe(TopicToolResult, func(Topic, Payload) { got = append(got, "first") })
	d2 := b.Subscribe(TopicToolResult, func(Topic, Payload) { got = append(got, "second") })
	d3 := b.Subscribe(TopicToolResult, func(Topic, Payload) { got = append(got, "third") })

	// Dispose the first and last registrations out of order.
	d1()
	d3()
	got = nil
	b.Emit(TopicToolResult, nil)
	if len(got) != 1 || got[0] != "second" {
		t.Fatalf("expected only the middle handler to run, got %v", got)
	}
	if n := b.Subscribers(TopicToolResult); n != 1 {
		t.Fatalf("expected 1 subscriber, got %d", n)
	}

	// The middle handler must still be removable after its neighbours are
	// disposed (the old index-based disposer turned it into a no-op).
	d2()
	if n := b.Subscribers(TopicToolResult); n != 0 {
		t.Fatalf("expected the remaining handler to be removable, got %d subscribers", n)
	}
	got = nil
	b.Emit(TopicToolResult, nil)
	if len(got) != 0 {
		t.Fatalf("no handler should run after full teardown, got %v", got)
	}
}

func TestDisposeDuringEmitIsSafe(t *testing.T) {
	// A handler may dispose another subscription mid-emit; iterating the
	// copied list keeps the run well-defined.
	b := NewBus()
	var calls int
	var d2 func()
	b.Subscribe(TopicToolResult, func(Topic, Payload) { calls++ })
	d2 = b.Subscribe(TopicToolResult, func(Topic, Payload) {
		calls++
		d2() // dispose itself mid-emit
	})
	b.Emit(TopicToolResult, nil)
	if calls != 2 {
		t.Fatalf("expected 2 handler calls in the first emit, got %d", calls)
	}
	b.Emit(TopicToolResult, nil)
	if calls != 3 {
		t.Fatalf("expected only the first handler after self-dispose, got %d calls", calls)
	}
	if n := b.Subscribers(TopicToolResult); n != 1 {
		t.Fatalf("expected 1 subscriber after mid-emit dispose, got %d", n)
	}
}

func TestEmitConcurrentWithDispose(t *testing.T) {
	// Run with -race: Emit iterates a copy while disposers move elements in
	// the original slice; the two must never race.
	b := NewBus()
	b.Subscribe(TopicToolResult, func(Topic, Payload) {})
	b.Subscribe(TopicMessageAdded, func(Topic, Payload) {})

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			d := b.Subscribe(TopicToolResult, func(Topic, Payload) {})
			if i%2 == 0 {
				d()
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			b.Emit(TopicToolResult, nil)
			b.Emit(TopicMessageAdded, nil)
		}
	}()
	wg.Wait()
}
