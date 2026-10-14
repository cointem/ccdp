package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/tooling"
)

// dispatchTools executes a batch of tool calls. With maxParallelTools > 1 the
// calls run concurrently (bounded worker pool); approvals stay serialized so
// the user only ever sees one modal. Results come back in call order.
func (a *Agent) dispatchTools(calls []messages.ToolCall) []toolRunResult {
	return a.dispatchToolsForContext(calls, toolJournalContext{})
}

func (a *Agent) dispatchToolsForContext(calls []messages.ToolCall, jc toolJournalContext) []toolRunResult {
	n := jc.maxParallel
	if n == 0 {
		a.mu.Lock()
		n = a.services.cfg.MaxParallelTools
		a.mu.Unlock()
	}
	if n < 1 {
		n = 1
	}
	calls = messages.Clone([]messages.Message{{ToolCalls: calls}})[0].ToolCalls
	invocations := make([]tooling.Invocation, len(calls))
	for i, call := range calls {
		invocations[i] = toolInvocation{agent: a, call: call, journal: withToolCallIndex(jc, i)}
	}
	executed := (tooling.Executor{MaxParallel: n}).Execute(invocations)
	results := make([]toolRunResult, len(executed))
	for i, result := range executed {
		results[i] = toolRunResult{output: result.Output, isErr: result.IsError, status: result.Status}
	}

	if jc.enabled {
		_, _ = a.projectToolResults(jc, calls, results)
	}
	return results
}

func withToolCallIndex(jc toolJournalContext, index int) toolJournalContext {
	jc.callIndex = index
	return jc
}

func withToolCallStatus(jc toolJournalContext, sink *string) toolJournalContext {
	jc.statusSink = sink
	return jc
}

type toolRunResult struct {
	output string
	isErr  bool
	status string // explicit success/error/denied/cancelled outcome
}

// toolInvocation bridges runtime admission and journal settlement into the
// scheduler. It retains the frozen call index even when reads finish out of order.
type toolInvocation struct {
	agent   *Agent
	call    messages.ToolCall
	journal toolJournalContext
}

func (c toolInvocation) ReadOnly() bool { return c.agent.toolCallIsReadOnly(c.journal, c.call) }

func (c toolInvocation) Execute() tooling.Result {
	a := c.agent
	if a.interrupted() {
		output := "interrupted before execution"
		if err := a.finishUnexecutedToolWithStatus(c.journal, c.call, output, true, "cancelled"); err != nil {
			output = "session persistence failed: " + err.Error()
		}
		return tooling.Result{Output: output, IsError: true, Status: "cancelled"}
	}
	var status string
	out, failed := a.executeToolWithJournal(c.call, withToolCallStatus(c.journal, &status))
	if status == "" {
		if failed {
			status = "error"
		} else {
			status = "success"
		}
	}
	return tooling.Result{Output: out, IsError: failed, Status: status}
}
