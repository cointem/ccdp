package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/tooling"
)

// dispatchToolsForContext settles a batch and projects its model output in
// call order, independently of the order in which concurrent reads finish.
func (a *Agent) dispatchToolsForContext(calls []messages.ToolCall, jc toolJournalContext) ([]tooling.Result, error) {
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
	results := (tooling.Executor{MaxParallel: n}).Execute(invocations)

	_, err := a.projectToolResults(jc, calls, results)
	return results, err
}

func withToolCallIndex(jc toolJournalContext, index int) toolJournalContext {
	jc.callIndex = index
	return jc
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
	return a.executeToolResultWithJournal(c.call, c.journal)
}
