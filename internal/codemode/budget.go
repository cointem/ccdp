package codemode

import (
	"context"
	"sync"
	"time"
)

type budgetKey struct{}
type budget struct {
	mu        sync.Mutex
	paused    int
	remaining time.Duration
	last      time.Time
}

// Pause suspends only the cell's wall-clock budget during explicit human
// approval. Parent cancellation remains active. Nested pauses are supported.
func Pause(ctx context.Context) func() {
	b, _ := ctx.Value(budgetKey{}).(*budget)
	if b == nil {
		return func() {}
	}
	b.mu.Lock()
	b.tick()
	b.paused++
	b.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.tick(); b.paused--; b.mu.Unlock() }) }
}
func (b *budget) tick() {
	now := time.Now()
	if b.paused == 0 {
		b.remaining -= now.Sub(b.last)
	}
	b.last = now
}
func withBudget(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	b := &budget{remaining: d, last: time.Now()}
	ctx, cancel := context.WithCancelCause(context.WithValue(parent, budgetKey{}, b))
	go func() {
		t := time.NewTicker(25 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.mu.Lock()
				b.tick()
				expired := b.remaining <= 0
				b.mu.Unlock()
				if expired {
					cancel(context.DeadlineExceeded)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled) }
}
