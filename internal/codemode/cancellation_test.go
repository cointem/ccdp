package codemode

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCancellationBoundsIgnoringCallbackAndStopsQueue(t *testing.T) {
	if !Available {
		t.Skip("requires Darwin Seatbelt")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	var later atomic.Int32
	done := make(chan struct{})
	var summary Summary
	var err error
	go func() {
		summary, err = Run(ctx, Options{Code: `await Promise.all([tools.Block({}),tools.Later({})])`, Timeout: 10 * time.Second, MaxParallel: 1, Bindings: map[string]Binding{
			"Block": {Run: func(context.Context, int, map[string]any) Result {
				close(started)
				defer close(returned)
				<-release
				return Result{Status: "success"}
			}},
			"Later": {Run: func(context.Context, int, map[string]any) Result { later.Add(1); return Result{Status: "success"} }},
		}})
		close(done)
	}()
	select {
	case <-started:
	case <-done:
		t.Fatalf("worker failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timed out")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung after cancellation")
	}
	if err == nil || !strings.Contains(err.Error(), "outcomes are unknown") || summary.Unknown != 1 || later.Load() != 0 {
		t.Fatalf("summary=%+v err=%v later=%d", summary, err, later.Load())
	}
	t.Cleanup(func() {
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Error("late callback blocked")
		}
	})
}

func TestDocumentedFrameAndEmissionBudgets(t *testing.T) {
	if !Available {
		t.Skip("requires Darwin Seatbelt")
	}
	calls := 0
	s, err := runTest(t, `await tools.Echo({text:'中'.repeat(120000)});for(let i=0;i<4;i++)text('x'.repeat(600*1024));`, map[string]Binding{
		"Echo": {Run: func(context.Context, int, map[string]any) Result { calls++; return Result{Status: "success"} }},
	})
	if err != nil || calls != 1 || len(s.Outputs) != 4 {
		t.Fatalf("documented limits rejected: calls=%d outputs=%d err=%v", calls, len(s.Outputs), err)
	}
	s, err = runTest(t, `try{await tools.Echo({text:'😀'.repeat(270000)})}catch(e){text(e.message)}`, map[string]Binding{
		"Echo": {Run: func(context.Context, int, map[string]any) Result {
			t.Error("oversized UTF-8 frame executed")
			return Result{Status: "success"}
		}},
	})
	if err != nil || s.Calls != 0 || len(s.Outputs) != 1 || !strings.Contains(string(s.Outputs[0]), "frame limit") {
		t.Fatalf("UTF-8 limit: %+v %v", s, err)
	}
}
