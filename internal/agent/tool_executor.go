package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/tooling"
)

// executeTool runs one tool call, handling Go hooks, shell hooks, permission
// and approval.
func (a *Agent) executeTool(tc messages.ToolCall) (string, bool) {
	return a.executeToolWithJournal(tc, toolJournalContext{})
}

// executeToolWithJournal is the canonical dispatch path. The compatibility
// wrapper above keeps direct package callers on the pre-typed event behavior.
func (a *Agent) executeToolWithJournal(tc messages.ToolCall, journal toolJournalContext) (output string, isErr bool) {
	var execution *toolJournalExecution
	if journal.enabled {
		execution = &toolJournalExecution{ctx: journal}
		defer func() {
			if execution.finished {
				return
			}
			// No tool crosses the durable start boundary on a gate/approval
			// rejection. Derive that state from the control path, never from
			// the tool's human-readable output (which may legitimately contain
			// words such as "blocked" after a real execution). A cancelled
			// turn is distinct from a denied call for recovery/UI purposes.
			if execution.status == "" && !execution.started {
				if a.interrupted() {
					execution.setStatus("cancelled")
				} else {
					execution.setStatus("denied")
				}
			}
			if err := execution.finish(a, tc, output, isErr); err != nil {
				execution.setStatus("error")
				output = "session persistence failed: " + err.Error()
				isErr = true
			}
			output = a.toolOutputPreview(output, 0)
		}()
	}
	result := (tooling.Executor{}).Run(&toolAuthority{agent: a, call: tc, journal: journal, execution: execution})
	return result.Output, result.IsError
}
