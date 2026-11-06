package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/protocol"
	"ccdp/internal/tooling"
)

type Binding struct {
	Concurrent func(map[string]any) bool
	Process    bool // trusted host implementation owns session process handles
	Run        func(context.Context, int, map[string]any) Result
}
type Options struct {
	Code        string
	Timeout     time.Duration
	MaxParallel int
	Bindings    map[string]Binding
}
type Summary = protocol.CodeResult
type completion struct {
	request queuedCall
	result  Result
}
type queuedCall struct {
	frame
	concurrent bool
}
type incoming struct {
	frame frame
	err   error
}

// Cancellation is cooperative, so a broken host adapter must not hold the
// worker and its owner forever. Agent adapters settle uncertain tool outcomes
// within CancelGrace; the host allows another grace period for journaling.
const CancelGrace = time.Second

type boundedDiagnostic struct {
	mu sync.Mutex
	b  []byte
}

func (b *boundedDiagnostic) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > 4096-len(b.b) {
		p = p[:4096-len(b.b)]
	}
	b.b = append(b.b, p...)
	return n, nil
}
func (b *boundedDiagnostic) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.b) }

// Run owns the worker, bounded queue and every admitted child until settlement.
// A parent never consumes one of its children's worker permits.
func Run(parent context.Context, o Options) (summary Summary, err error) {
	summary.Outputs = []json.RawMessage{}
	processes := map[string]bool{}
	defer func() {
		if err != nil {
			summary.Error = err.Error()
		}
		if len(summary.Outputs) == 0 {
			summary.Note = "No data emitted; call text(value) to return data."
		}
		for id, running := range processes {
			if running {
				summary.Processes = append(summary.Processes, id)
			}
		}
		sort.Strings(summary.Processes)
	}()
	if len(o.Code) == 0 || len(o.Code) > MaxCode {
		return summary, fmt.Errorf("code must contain 1..%d bytes", MaxCode)
	}
	if !Available {
		return summary, errors.New("RunCode requires the Darwin Seatbelt backend")
	}
	if o.Timeout == 0 {
		o.Timeout = 120 * time.Second
	}
	if o.MaxParallel < 1 {
		o.MaxParallel = 1
	}
	if o.MaxParallel > 16 {
		o.MaxParallel = 16
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	p, err := execution.CodeWorker(ctx, WorkerArgument)
	if err != nil {
		return summary, err
	}
	stdin, err := p.StdinPipe()
	if err != nil {
		return summary, err
	}
	defer stdin.Close()
	stdout, err := p.StdoutPipe()
	if err != nil {
		return summary, err
	}
	defer stdout.Close()
	var diagnostic boundedDiagnostic
	p.SetStderr(&diagnostic)
	if err = p.Start(); err != nil {
		return summary, err
	}
	// Executor preparation has its own bounded startup. Charge only the
	// running cell against its wall-clock budget, as for shell invocations.
	ctx, cancelBudget := withBudget(ctx, o.Timeout)
	defer cancelBudget()
	defer func() {
		cancel()
		if stopErr := p.Stop("code cell ended"); stopErr != nil {
			err = errors.Join(err, stopErr)
		}
		// Wait for confirmed reclamation even if Stop reported a timeout.
		_ = p.Wait()
	}()
	write := func(f frame) error {
		var b bytes.Buffer
		if e := writeFrame(&b, f); e != nil {
			return e
		}
		return p.WriteInput(b.Bytes())
	}
	names := make([]string, 0, len(o.Bindings))
	for name := range o.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	if err = write(frame{Type: "start", Version: 1, Code: o.Code, Names: names}); err != nil {
		return summary, err
	}
	// Keep protocol directions independent. A large result must not block
	// draining a worker that is simultaneously submitting a batch of calls.
	writes := make(chan frame, MaxQueue+16)
	writeErrors := make(chan error, 1)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-writes:
				if e := write(f); e != nil {
					writeErrors <- e
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-writeDone }()
	frames := make(chan incoming, 1)
	readDone := make(chan struct{})
	defer func() { stdout.Close(); <-readDone }()
	go func() {
		defer close(readDone)
		for {
			f, e := readFrame(stdout)
			select {
			case frames <- incoming{f, e}:
			case <-ctx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	finished := make(chan completion, 16)
	queue := []queuedCall{}
	gate := tooling.Barrier{Limit: o.MaxParallel}
	nextID := 1
	closing := false
	var shutdown *time.Timer
	var shutdownDeadline <-chan time.Time
	defer func() {
		if shutdown != nil {
			shutdown.Stop()
		}
	}()
	// Captured emissions and program results share one runtime budget.
	// Include the output array brackets and each comma as well as encoded data.
	resultBytes := 2
	closeCell := func(e error) {
		if !closing {
			closing = true
			err = e
			cancel()
			summary.Cancelled += len(queue)
			queue = nil
			if gate.Active > 0 {
				shutdown = time.NewTimer(2 * CancelGrace)
				shutdownDeadline = shutdown.C
			}
		}
	}
	launch := func(f queuedCall) {
		binding := o.Bindings[f.Name]
		gate.Start(f.concurrent)
		go func() {
			var r Result
			defer func() {
				if v := recover(); v != nil {
					r = Result{Status: "unknown", Text: fmt.Sprintf("tool callback panicked: %v", v)}
				}
				finished <- completion{f, r}
			}()
			r = binding.Run(ctx, f.ID, f.Args)
			if r.DataError == "" {
				if dataErr := ValidateData(r.Data); dataErr != nil {
					r.DataError = dataErr.Error()
				}
			}
			if r.DataError != "" {
				r.Data = nil
			}
		}()
	}
	for {
		if !closing && ctx.Err() != nil {
			closeCell(context.Cause(ctx))
		}
		for !closing && len(queue) > 0 && gate.CanStart(queue[0].concurrent) {
			f := queue[0]
			queue = queue[1:]
			launch(f)
		}
		if closing && gate.Active == 0 && len(queue) == 0 {
			break
		}
		var read <-chan incoming = frames
		var cancelled <-chan struct{} = ctx.Done()
		if closing {
			read = nil
			cancelled = nil
		}
		select {
		case <-shutdownDeadline:
			// At most MaxParallel callbacks can remain. The buffered completion
			// channel lets a late return finish without blocking on this owner.
			summary.Unknown += gate.Active
			return summary, errors.Join(err, errors.New("tool cancellation timed out; in-flight outcomes are unknown; do not repeat side effects"))
		case e := <-writeErrors:
			closeCell(e)
		case <-cancelled:
			closeCell(context.Cause(ctx))
		case in := <-read:
			if in.err != nil {
				if errors.Is(in.err, io.EOF) {
					closeCell(fmt.Errorf("worker exited without completion: %s", diagnostic.String()))
				} else {
					closeCell(in.err)
				}
				continue
			}
			f := in.frame
			switch f.Type {
			case "limit":
				closeCell(errors.New("worker resource limit: " + f.Error))
			case "call":
				if f.ID != nextID || f.ID > MaxCalls || f.Args == nil || o.Bindings[f.Name].Run == nil {
					closeCell(errors.New("invalid or unauthorized worker tool request"))
					continue
				}
				if len(queue) >= MaxQueue {
					closeCell(errors.New("code tool queue limit exceeded"))
					continue
				}
				nextID++
				summary.Calls++
				classify := o.Bindings[f.Name].Concurrent
				queue = append(queue, queuedCall{frame: f, concurrent: classify != nil && classify(f.Args)})
			case "emit":
				value, encodeErr := json.Marshal(f.Value)
				if len(f.Value) == 0 || encodeErr != nil {
					closeCell(errors.New("invalid text output"))
					continue
				}
				size := len(value)
				if len(summary.Outputs) > 0 {
					size++
				}
				if resultBytes+size > MaxResults {
					closeCell(errors.New("program result budget exceeded"))
					continue
				}
				resultBytes += size
				summary.Outputs = append(summary.Outputs, value)
			case "done":
				if f.Error != "" {
					closeCell(errors.New(f.Error))
				} else if gate.Active > 0 || len(queue) > 0 {
					closeCell(errors.New("incomplete: script ended with unawaited tool calls; admitted calls were cancelled and settled"))
				} else {
					closeCell(nil)
				}
			default:
				closeCell(errors.New("unexpected worker message"))
			}
		case c := <-finished:
			gate.Finish(c.request.concurrent)
			switch c.result.Status {
			case "success":
			case "denied":
				summary.Denied++
			case "cancelled":
				summary.Cancelled++
			case "unknown":
				summary.Unknown++
			default:
				summary.Failed++
			}
			if p := c.result.Process; o.Bindings[c.request.Name].Process && p != nil {
				processes[p.ID] = !p.Terminal()
				if len(c.result.CompletionIDs) > 0 {
					duplicate := false
					for _, old := range summary.Completed {
						if old.ID == p.ID {
							duplicate = true
						}
					}
					if !duplicate {
						summary.Completed = append(summary.Completed, p.Reference())
					}
				}
			}
			b, e := json.Marshal(c.result)
			if e != nil || len(b) > MaxResult {
				closeCell(errors.New("program tool result exceeds 512 KiB or is not valid JSON"))
			}
			resultBytes += len(b)
			if resultBytes > MaxResults {
				closeCell(errors.New("program result budget exceeded"))
			}
			if !closing {
				select {
				case writes <- frame{Type: "result", ID: c.request.ID, Result: &c.result}:
				default:
					closeCell(errors.New("worker response queue limit exceeded"))
				}
			}
		}
	}
	return summary, err
}
