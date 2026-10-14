package agent

import (
	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

// executeToolCallsForStep runs the batch of tool calls admitted this step and
// returns them in call order with any events deferred while they were in
// flight. A reply cut off by the token limit never executes its "salvaged" but
// incomplete calls; those are returned as errors so the model re-issues them.
func (a *Agent) executeToolCallsForStep(calls []messages.ToolCall, step stepRuntime, res llm.StreamResult) ([]toolRunResult, []Event) {
	a.mu.Lock()
	a.publications.deferToolEvents = true
	a.publications.deferredToolEvents = nil
	a.mu.Unlock()
	journal := toolJournalContextForStep(step)
	var results []toolRunResult
	if res.FinishReason == "length" && len(calls) > 0 {
		a.emitStatus("reply truncated by the token limit — voiding %d incomplete tool call(s)", len(calls))
		results = make([]toolRunResult, len(calls))
		for i, tc := range calls {
			out := "not executed: the assistant message hit the token limit before this tool call finished streaming, " +
				"so its arguments may be incomplete. Re-issue the tool call with the full arguments."
			results[i] = toolRunResult{output: out, isErr: true}
			if err := a.finishUnexecutedToolWithStatus(withToolCallIndex(journal, i), tc, out, true, "denied"); err != nil {
				results[i].output = "session persistence failed: " + err.Error()
			}
			results[i].status = "denied"
			a.emit(toolEvent(tc, "denied", "voided (message truncated)"))
		}
		if journal.enabled {
			_, _ = a.projectToolResults(journal, calls, results)
		}
	} else {
		results = a.dispatchToolsForContext(calls, journal)
	}
	a.mu.Lock()
	a.publications.deferToolEvents = false
	deferredToolEvents := append([]Event(nil), a.publications.deferredToolEvents...)
	a.publications.deferredToolEvents = nil
	a.mu.Unlock()
	return results, deferredToolEvents
}
