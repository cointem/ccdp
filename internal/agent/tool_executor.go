package agent

import (
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tooling"
)

// executeToolResultWithJournal runs one call through admission, hooks and
// execution, recording its outcome before returning the structured result.
func (a *Agent) executeToolResultWithJournal(tc messages.ToolCall, journal toolJournalContext) (result tooling.Result) {
	a.emitToolPhase(tc, protocol.ExecutionQueued)
	execution := &toolJournalExecution{ctx: journal}
	defer func() {
		result.Failure = execution.failure
		ev := toolEvent(tc, result.Status, "")
		ev.Kind, ev.Tool.Phase, ev.Tool.Failure = protocol.EventToolPhase, protocol.ExecutionCompleted, execution.failure
		a.emit(ev)
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			result = tooling.Result{Output: fmt.Sprintf("tool execution panicked; outcome unknown: %v", recovered), IsError: true}
			if !execution.finished {
				execution.setStatus("unknown")
			}
		}
		if execution.finished {
			result.Status = execution.status
			result.OutputID = execution.outputID
			return
		}
		// A rejected call never crossed the durable start boundary. Its
		// status follows the control path rather than the output text.
		if execution.status == "" && !execution.started {
			if a.interrupted() {
				execution.setStatus("cancelled")
			} else {
				execution.setStatus("denied")
			}
		}
		if err := execution.finish(a, tc, result.Output, result.IsError); err != nil {
			execution.setStatus("error")
			result = tooling.Result{Output: "session persistence failed: " + err.Error(), IsError: true}
		}
		result.Status = execution.status
		result.OutputID = execution.outputID
		if !journal.program && result.Code == nil {
			preview := a.toolOutputPreview(result.Output, 0, execution.outputID)
			result.DisplayTruncated = result.DisplayTruncated || preview != result.Output
			result.Output = preview
		}
	}()
	return (tooling.Executor{}).Run(&toolAuthority{agent: a, call: tc, journal: journal, execution: execution})
}
