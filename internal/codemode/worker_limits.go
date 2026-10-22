package codemode

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const (
	workerExecutionBudget = 5 * time.Second
	workerMemoryTarget    = 96 << 20
	workerHeapBudget      = 128 << 20
	workerLimitGrace      = 250 * time.Millisecond
)

// Goja shares the Go heap. The soft GC target and sampled heap watchdog are
// process-wide, not an exact per-VM allocation cap. This guard is only created
// in the disposable worker. Large individual allocations can overshoot it.
type workerLimits struct {
	vm       *goja.Runtime
	mu       sync.Mutex
	entered  time.Time
	spent    time.Duration
	fatal    error
	failedAt time.Time
	stop     chan struct{}
	done     chan struct{}
}

func newWorkerLimits(vm *goja.Runtime) *workerLimits {
	l := &workerLimits{vm: vm, stop: make(chan struct{}), done: make(chan struct{})}
	go l.watch()
	return l
}

func (l *workerLimits) close() {
	close(l.stop)
	<-l.done
}

func (l *workerLimits) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fatal == nil {
		l.fatal = err
		l.failedAt = time.Now()
		l.vm.Interrupt(err)
	}
}

func (l *workerLimits) run(fn func() error) (err error) {
	l.mu.Lock()
	if l.fatal != nil {
		err = l.fatal
		l.mu.Unlock()
		return err
	}
	l.entered = time.Now()
	l.mu.Unlock()
	defer func() {
		// Some native value operations can panic with a JS exception. Never
		// format that value outside the watchdog's execution window.
		if recovered := recover(); recovered != nil {
			err = errors.New("JavaScript runtime operation failed")
		}
		l.mu.Lock()
		l.spent += time.Since(l.entered)
		l.entered = time.Time{}
		if l.fatal != nil {
			err = l.fatal
		} else if l.spent >= workerExecutionBudget {
			err = errors.New("JavaScript execution limit exceeded")
		}
		l.mu.Unlock()
	}()
	err = fn()
	if err != nil {
		// Exception.Error may invoke model-controlled toString. Format it
		// while the watchdog is active and return an inert Go error.
		message := "JavaScript execution failed"
		if ex := l.vm.Try(func() { message = err.Error() }); ex == nil {
			err = errors.New(message[:min(len(message), 4096)])
		} else {
			err = errors.New("JavaScript exception could not be formatted")
		}
	}
	return err
}

func (l *workerLimits) watch() {
	defer close(l.done)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
		}
		l.mu.Lock()
		spent := l.spent
		if !l.entered.IsZero() {
			spent += time.Since(l.entered)
		}
		fatal, failedAt := l.fatal, l.failedAt
		l.mu.Unlock()
		if fatal != nil {
			if time.Since(failedAt) >= workerLimitGrace {
				// Interrupt cannot stop a blocking native operation. Kill only
				// this worker; the parent settles admitted tool calls normally.
				fmt.Fprintln(os.Stderr, "worker resource limit:", fatal)
				os.Exit(70)
			}
			continue
		}
		if spent >= workerExecutionBudget {
			l.fail(errors.New("JavaScript execution limit exceeded"))
			continue
		}
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		if stats.HeapAlloc > workerHeapBudget {
			l.fail(errors.New("worker Go heap memory budget exceeded (128 MiB)"))
		}
	}
}
