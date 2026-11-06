package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ccdp/internal/codemode"
	"ccdp/internal/execution"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func (a *Agent) runCode(ctx context.Context, parent messages.ToolCall, jc toolJournalContext, code string, timeout time.Duration) (protocol.CodeResult, error) {
	if jc.program {
		return protocol.CodeResult{}, fmt.Errorf("RunCode cannot be called recursively")
	}
	ctx = execution.WithController(ctx, a.configSnapshot().ExecutorID)
	cellCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bindings := map[string]codemode.Binding{}
	var denialMu sync.Mutex
	var denialKey string
	mixedResults := false
	for _, name := range jc.allowedTools {
		t, ok := jc.toolLease.Get(name)
		definition, defined := jc.toolLease.Definition(name)
		if !ok || !defined || !definition.ProgramCallable {
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
		bindings[name] = codemode.Binding{Concurrent: func(args map[string]any) bool {
			return invocationKind(t, args).Concurrent()
		}, Process: processTool, Run: func(childCtx context.Context, index int, args map[string]any) codemode.Result {
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
			denialMu.Lock()
			if result.Failure == nil || result.Failure.DecisionKey == "" {
				mixedResults = true
			} else if denialKey != "" && denialKey != result.Failure.DecisionKey {
				mixedResults = true
			} else {
				denialKey = result.Failure.DecisionKey
			}
			denialMu.Unlock()
			if a.persistenceFailure() != nil || a.codeSteerPending() || result.Status == "resource_limit" || result.Status == "unknown" {
				cancel()
			}
			return codemode.Result{Process: result.Process, CompletionIDs: result.CompletionIDs, CallID: id, Status: result.Status, Text: result.Output, Data: result.Data, Truncated: result.DataTruncated, DataError: result.DataError, OutputID: result.OutputID}
		}}
	}
	result, err := codemode.Run(cellCtx, codemode.Options{Code: code, Timeout: timeout, MaxParallel: jc.maxParallel, Bindings: bindings})
	denialMu.Lock()
	if !mixedResults && result.Calls > 0 && result.Calls == result.Denied {
		result.DenialKey = denialKey
	}
	denialMu.Unlock()
	return result, err
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
