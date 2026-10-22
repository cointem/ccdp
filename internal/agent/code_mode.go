package agent

import (
	"context"
	"fmt"
	"time"

	"ccdp/internal/codemode"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func (a *Agent) runCode(ctx context.Context, parent messages.ToolCall, jc toolJournalContext, code string, timeout time.Duration) (protocol.CodeResult, error) {
	if jc.program {
		return protocol.CodeResult{}, fmt.Errorf("RunCode cannot be called recursively")
	}
	cellCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bindings := map[string]codemode.Binding{}
	for _, name := range jc.allowedTools {
		t, ok := jc.toolLease.Get(name)
		definition, defined := jc.toolLease.Definition(name)
		if !ok || !defined || !definition.ProgramCallable {
			continue
		}
		if a.inPlanMode() && !planAllowedImplementation(t) {
			continue
		}
		if denied, _ := childToolEligibility(a, name, t); denied {
			continue
		}
		processTool := false
		switch t.(type) {
		case *tools.BashTool, *tools.ProcessTool:
			processTool = true
		}
		bindings[name] = codemode.Binding{ReadOnly: toolKind(t) == permissions.KindRead, Process: processTool, Run: func(childCtx context.Context, index int, args map[string]any) codemode.Result {
			if a.codeSteerPending() {
				cancel()
			}
			id := messages.StableID("code-child", struct {
				Session, Turn, Step, Parent string
				Index                       int
			}{a.conversation.sessionID, jc.turnID, jc.stepID, parent.ID, index})
			call := messages.ToolCall{ID: id, Name: name, Arguments: args, ParentCallID: parent.ID, ChildIndex: index}
			child := jc
			child.program = true
			child.parentCallID = parent.ID
			child.childIndex = index
			child.context = childCtx
			result := a.executeToolResultWithJournal(call, child)
			if a.persistenceFailure() != nil || a.codeSteerPending() || result.Status == "resource_limit" || result.Status == "unknown" {
				cancel()
			}
			return codemode.Result{CallID: id, Status: result.Status, Text: result.Output, Data: result.Data, Truncated: result.DataTruncated, DataError: result.DataError, OutputID: result.OutputID}
		}}
	}
	return codemode.Run(cellCtx, codemode.Options{Code: code, Timeout: timeout, MaxParallel: jc.maxParallel, Bindings: bindings})
}

func (a *Agent) codeSteerPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, input := range a.conversation.durable.PendingInputViews() {
		if input.Strategy == protocol.InputSteer {
			return true
		}
	}
	return false
}
