package tooling

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type invocation struct {
	read bool
	run  func() Result
}

func (i invocation) ReadOnly() bool { return i.read }

func (i invocation) Execute() Result { return i.run() }

func TestReadGroupsRespectWriteBarriersAndCallOrder(t *testing.T) {
	var active atomic.Int32
	var completed atomic.Int32
	var writeDone atomic.Bool
	firstStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	calls := []Invocation{}
	for i := 0; i < 2; i++ {
		calls = append(calls, invocation{read: true, run: func() Result {
			active.Add(1)
			firstStarted <- struct{}{}
			<-release
			active.Add(-1)
			completed.Add(1)
			return Result{Output: fmt.Sprint(i)}
		}})
	}
	calls = append(calls, invocation{run: func() Result {
		if active.Load() != 0 || completed.Load() != 2 {
			t.Error("write overlapped preceding reads")
		}
		writeDone.Store(true)
		return Result{Output: "write"}
	}})
	calls = append(calls, invocation{read: true, run: func() Result {
		if !writeDone.Load() {
			t.Error("read overtook write")
		}
		return Result{Output: "last"}
	}})
	done := make(chan []Result, 1)
	go func() { done <- (Executor{MaxParallel: 2}).Execute(calls) }()
	<-firstStarted
	<-firstStarted
	releaseOnce.Do(func() { close(release) })
	results := <-done
	for i, want := range []string{"0", "1", "write", "last"} {
		if results[i].Output != want {
			t.Fatalf("result %d=%q want %q", i, results[i].Output, want)
		}
	}
}
