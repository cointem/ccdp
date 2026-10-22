// Package tooling schedules frozen tool invocations. It does not own session
// state; admission and durable completion remain explicit invocation boundaries.
package tooling

import (
	"ccdp/internal/protocol"
	"encoding/json"
)

// Invocation owns one frozen call and its durable completion path. Execute
// must settle even a cancelled call: dropping queued work would leave an
// admitted model step without its ordered tool results.
type Invocation interface {
	ReadOnly() bool
	Execute() Result
}

type Result struct {
	Output           string
	Code             *protocol.CodeResult
	IsError          bool
	Status           string
	Data             json.RawMessage
	DataTruncated    bool
	DisplayTruncated bool
	DataError        string
	OutputID         string
}

type Executor struct{ MaxParallel int }

// Execute parallelizes consecutive read-only calls. Every other call is a
// barrier: all previous reads finish before it starts, and subsequent reads
// start only after it finishes. Output always follows the original call order.
func (e Executor) Execute(calls []Invocation) []Result {
	results := make([]Result, len(calls))
	gate := Barrier{Limit: e.MaxParallel}
	type completed struct {
		index  int
		read   bool
		result Result
	}
	done := make(chan completed, max(1, e.MaxParallel))
	next := 0
	for next < len(calls) || gate.Active > 0 {
		for next < len(calls) && gate.CanStart(calls[next].ReadOnly()) {
			i := next
			next++
			read := calls[i].ReadOnly()
			gate.Start(read)
			go func() { done <- completed{i, read, calls[i].Execute()} }()
		}
		c := <-done
		gate.Finish(c.read)
		results[c.index] = c.result
	}
	return results
}
