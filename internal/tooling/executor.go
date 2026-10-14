// Package tooling schedules frozen tool invocations. It does not own session
// state; admission and durable completion remain explicit invocation boundaries.
package tooling

import (
	"sync"
)

// Invocation owns one frozen call and its durable completion path. Execute
// must settle even a cancelled call: dropping queued work would leave an
// admitted model step without its ordered tool results.
type Invocation interface {
	ReadOnly() bool
	Execute() Result
}

type Result struct {
	Output  string
	IsError bool
	Status  string
}

type Executor struct{ MaxParallel int }

// Execute parallelizes consecutive read-only calls. Every other call is a
// barrier: all previous reads finish before it starts, and subsequent reads
// start only after it finishes. Output always follows the original call order.
func (e Executor) Execute(calls []Invocation) []Result {
	n := max(1, e.MaxParallel)
	results := make([]Result, len(calls))
	for start := 0; start < len(calls); {
		if !calls[start].ReadOnly() {
			results[start] = calls[start].Execute()
			start++
			continue
		}
		end := start + 1
		for end < len(calls) && calls[end].ReadOnly() {
			end++
		}
		workers := min(n, end-start)
		if workers == 1 {
			for i := start; i < end; i++ {
				results[i] = calls[i].Execute()
			}
		} else {
			jobs := make(chan int)
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Go(func() {
					for i := range jobs {
						results[i] = calls[i].Execute()
					}
				})
			}
			for i := start; i < end; i++ {
				jobs <- i
			}
			close(jobs)
			wg.Wait()
		}
		start = end
	}
	return results
}
