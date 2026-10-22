package agent

import (
	"testing"

	"ccdp/internal/messages"
)

// These unit fixtures freeze the same tool bindings and write the same
// journal facts as a model step, without making a provider request.
func testToolStep(t *testing.T, a *Agent) stepRuntime {
	t.Helper()
	step, _ := a.captureStepRuntime()
	step.tools = a.toolDefsSnapshotFromLease(step.toolLease)
	t.Cleanup(func() { a.releaseStepContext(step) })
	return step
}

func executeTestTool(t *testing.T, a *Agent, call messages.ToolCall) (string, bool) {
	t.Helper()
	if call.ID == "" {
		call.ID = nextRuntimeID("test-tool")
	}
	step := testToolStep(t, a)
	result := a.executeToolResultWithJournal(call, toolJournalContextForStep(step))
	return result.Output, result.IsError
}
