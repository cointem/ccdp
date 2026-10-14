package supervision

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type observedSource struct {
	mu  sync.Mutex
	run ObservedRun
}

func (s *observedSource) AttachedRun() ObservedRun { s.mu.Lock(); defer s.mu.Unlock(); return s.run }
func (s *observedSource) Snapshot(context.Context) (protocol.SessionView, error) {
	return protocol.SessionView{SessionID: "child", RunID: s.AttachedRun().ID}, nil
}

type endedReader struct {
	calls  atomic.Int32
	closes atomic.Int32
}

func (r *endedReader) Watch(context.Context, protocol.Cursor) (protocol.Subscription, error) {
	r.calls.Add(1)
	ch := make(chan protocol.Update)
	close(ch)
	return &endedSubscription{ch: ch, reader: r}, nil
}

type endedSubscription struct {
	ch     chan protocol.Update
	reader *endedReader
	once   sync.Once
}

func (s *endedSubscription) Updates() <-chan protocol.Update { return s.ch }
func (s *endedSubscription) Close() error                    { s.once.Do(func() { s.reader.closes.Add(1) }); return nil }

func TestObserverClosedRuntimeBacksOffAndFollowsNextRun(t *testing.T) {
	reader := &endedReader{}
	source := &observedSource{run: ObservedRun{ID: "first", Status: "running", Runtime: reader}}
	sub, err := Watch(context.Background(), context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	<-sub.Updates()
	time.Sleep(240 * time.Millisecond)
	if calls := reader.calls.Load(); calls > 4 || calls == 0 {
		t.Fatalf("closed runtime retried without bounded backoff: %d", calls)
	}
	source.mu.Lock()
	source.run = ObservedRun{ID: "second", Status: "queued"}
	source.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case update := <-sub.Updates():
			if update.Snapshot != nil && update.Snapshot.RunID == "second" {
				if err := sub.Close(); err != nil {
					t.Fatal(err)
				}
				if reader.calls.Load() != reader.closes.Load() {
					t.Fatalf("observer leaked a subscription: watches=%d closes=%d", reader.calls.Load(), reader.closes.Load())
				}
				return
			}
		case <-deadline:
			t.Fatal("observer failed to follow continued run")
		}
	}
}

func TestObserverLifetimeClosesWithoutControllingRuntime(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	source := &observedSource{run: ObservedRun{ID: "one", Status: "queued"}}
	sub, err := Watch(context.Background(), lifetime, source)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for range sub.Updates() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer outlived its owner")
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
}
