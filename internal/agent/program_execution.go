package agent

import (
	"fmt"
	"time"

	"ccdp/internal/codemode"
	"ccdp/internal/tools"
)

// Only the owner of this wait publishes execution facts. A tool that ignores
// cancellation may return later, but cannot publish a second result or run
// post-execution hooks after this invocation has settled as unknown.
func executeProgramTool(tool tools.Tool, ctx *tools.Context) (tools.Result, error, bool) {
	type outcome struct {
		result  tools.Result
		err     error
		unknown bool
	}
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if p := recover(); p != nil {
				out.err = fmt.Errorf("tool panicked; outcome unknown: %v", p)
				out.unknown = true
			}
			done <- out
		}()
		out.result, out.err = tool.Execute(ctx, ctx.Args)
	}()
	select {
	case out := <-done:
		return out.result, out.err, out.unknown
	case <-ctx.Done():
	}
	timer := time.NewTimer(codemode.CancelGrace)
	defer timer.Stop()
	select {
	case out := <-done:
		return out.result, out.err, out.unknown
	case <-timer.C:
		return tools.Result{}, fmt.Errorf("tool did not stop after cancellation; outcome unknown; do not repeat the operation"), true
	}
}
