package codemode

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/dop251/goja"
)

//go:embed bootstrap.js
var bootstrap string

const Available = runtime.GOOS == "darwin"

// WorkerMain runs before configuration, credentials or plugins are loaded.
// Only this disposable process changes Go's memory target. No host APIs or
// Node modules are installed in the JavaScript runtime.
func WorkerMain(input io.Reader, output io.Writer) int {
	previous := debug.SetMemoryLimit(workerMemoryTarget)
	defer debug.SetMemoryLimit(previous)
	if err := worker(input, output); err != nil {
		_ = writeFrame(output, frame{Type: "done", Error: err.Error()})
		return 1
	}
	return 0
}

func worker(input io.Reader, output io.Writer) error {
	start, err := readFrame(input)
	if err != nil {
		return err
	}
	if start.Type != "start" || start.Version != 1 || len(start.Code) == 0 || len(start.Code) > MaxCode {
		return fmt.Errorf("invalid worker start")
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	limits := newWorkerLimits(vm)
	defer limits.close()

	// Only the VM goroutine accesses values, promises and the outbound queue.
	// Interrupt is the sole runtime method used by the watchdog goroutine.
	var queue [][]byte
	queuedBytes := 0
	rejected := make(map[*goja.Promise]bool)
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		if op == goja.PromiseRejectionHandle {
			delete(rejected, p)
		} else if len(rejected) >= 256 {
			limits.fail(errors.New("unhandled rejection limit exceeded"))
		} else {
			rejected[p] = true
		}
	})
	if err := vm.Set("__send", func(call goja.FunctionCall) goja.Value {
		encoded := call.Argument(0).String()
		if len(encoded) > MaxFrame || queuedBytes+len(encoded) > 16<<20 {
			limits.fail(errors.New("worker protocol output limit exceeded"))
			return goja.Undefined()
		}
		queue = append(queue, []byte(encoded))
		queuedBytes += len(encoded)
		return goja.Undefined()
	}); err != nil {
		return err
	}
	// Capture the original parser before user code can replace global JSON.
	parse, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
	if !ok {
		return errors.New("missing JSON parser")
	}
	if start.Names == nil {
		start.Names = []string{}
	}
	names, _ := json.Marshal(start.Names)
	source := strings.NewReplacer(
		"__NAMES__", string(names),
		"__MAX_FRAME__", strconv.Itoa(MaxFrame),
		"__MAX_RESULTS__", strconv.Itoa(MaxResults),
		"__MAX_CALLS__", strconv.Itoa(MaxCalls),
	).Replace(bootstrap)
	var receiver goja.Callable
	var root *goja.Promise
	if err = limits.run(func() error {
		value, e := vm.RunScript("bootstrap.js", source)
		if e != nil {
			return e
		}
		receiver, ok = goja.AssertFunction(value)
		if !ok {
			return errors.New("invalid bootstrap receiver")
		}
		// Keep model source line 1 at cell.js:1. Goja drains Promise jobs
		// when execution returns to Go; no separate Pump API is needed.
		value, e = vm.RunScript("cell.js", "(async () => {"+start.Code+"\n})()")
		if e != nil {
			return e
		}
		root, ok = value.Export().(*goja.Promise)
		if !ok {
			return errors.New("script did not return a Promise")
		}
		return nil
	}); err != nil {
		return err
	}
	pending := map[int]bool{}
	for {
		for _, encoded := range queue {
			var f frame
			if err := json.Unmarshal(encoded, &f); err != nil {
				return err
			}
			if f.Type == "call" {
				pending[f.ID] = true
			}
			if err := writeFrameBytes(output, encoded); err != nil {
				return err
			}
		}
		queue = nil
		queuedBytes = 0
		if root.State() == goja.PromiseStateRejected {
			return rejectionError(vm, limits, root.Result())
		}
		if root.State() == goja.PromiseStateFulfilled {
			for p := range rejected {
				return rejectionError(vm, limits, p.Result())
			}
			if len(pending) > 0 {
				return errors.New("incomplete: script ended with unawaited tool calls")
			}
			return writeFrame(output, frame{Type: "done"})
		}
		if len(pending) == 0 {
			return errors.New("incomplete: script is waiting on an unresolved Promise")
		}
		f, e := readFrame(input)
		if e != nil {
			return e
		}
		if f.Type != "result" || f.Result == nil || !pending[f.ID] {
			return errors.New("invalid worker response")
		}
		delete(pending, f.ID)
		encoded, e := json.Marshal(f)
		if e != nil {
			return e
		}
		if err = limits.run(func() error {
			// Parse JSON into JS-owned objects, never expose Go maps or structs.
			value, e := parse(goja.Undefined(), vm.ToValue(string(encoded)))
			if e != nil {
				return e
			}
			_, e = receiver(goja.Undefined(), value)
			return e
		}); err != nil {
			return err
		}
	}
}

func rejectionError(vm *goja.Runtime, limits *workerLimits, reason goja.Value) error {
	message := "JavaScript promise rejected"
	// Error properties and string conversion can run user getters/toString.
	// Keep them inside the same execution and memory budget as the script.
	if err := limits.run(func() error {
		if e := vm.Try(func() {
			message = reason.String()
			if object, ok := reason.(*goja.Object); ok {
				if stack := object.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
					message = stack.String()
				}
			}
		}); e != nil {
			return errors.New("JavaScript rejection could not be formatted")
		}
		return nil
	}); err != nil {
		return err
	}
	return errors.New(message[:min(len(message), 4096)])
}
