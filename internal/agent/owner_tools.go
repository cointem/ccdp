package agent

import (
	"fmt"

	"ccdp/internal/jsonvalue"
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
)

// The implementation is already pinned by the step lease. The owner rechecks
// admission and records ToolStarted before returning permission to dispatch.
type toolStartRequested struct {
	call    messages.ToolCall
	tool    tools.Tool
	journal toolJournalContext
	started bool
	result  chan error
}

func (toolStartRequested) ownerControlMessage() {}

func (a *Agent) admitToolStart(tc messages.ToolCall, tool tools.Tool, execution *toolJournalExecution) error {
	tc.Arguments = jsonvalue.Clone(tc.Arguments)
	request := toolStartRequested{call: tc, tool: tool, result: make(chan error, 1)}
	if execution != nil {
		request.journal, request.started = execution.ctx, execution.started
		request.journal.statusSink = nil
	}
	var err error
	if a.owner == nil {
		err = a.acceptToolStart(request)
	} else {
		a.ensureRun()
		select {
		case a.owner.control <- request:
		case <-a.owner.done:
			return session.ErrClosed
		}
		select {
		case err = <-request.result:
		case <-a.owner.done:
			return session.ErrClosed
		}
	}
	if err == nil && execution != nil && execution.ctx.enabled {
		execution.started = true
	}
	return err
}

func (a *Agent) acceptToolStart(request toolStartRequested) error {
	execution := &toolJournalExecution{ctx: request.journal, started: request.started}
	if err := a.admitToolStartOwned(request.call, request.tool, execution); err != nil {
		return err
	}
	a.mu.Lock()
	a.toolUses++
	a.mu.Unlock()
	return nil
}

type toolCompleted struct {
	fact   session.ToolFinished
	result chan error
}

func (toolCompleted) ownerControlMessage() {}

func (a *Agent) settleToolResult(fact session.ToolFinished) error {
	request := toolCompleted{fact: fact, result: make(chan error, 1)}
	if a.owner == nil {
		return a.commitToolResult(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

func (a *Agent) commitToolResult(request toolCompleted) error {
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(session.ErrClosed)
	}
	fact := request.fact
	if _, err := p.Commit(session.Batch{
		TransactionID: messages.StableID("tool-finished", struct{ TurnID, StepID, CallID string }{fact.TurnID, fact.StepID, fact.CallID}),
		Events:        []session.Event{fact},
	}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ToolFinished: %w", err))
	}
	return nil
}

type toolProjectionRequested struct {
	journal toolJournalContext
	calls   []messages.ToolCall
	results []toolRunResult
	result  chan toolProjectionResult
}

func (toolProjectionRequested) ownerControlMessage() {}

type toolProjectionResult struct {
	results []toolRunResult
	budget  session.Budget
	err     error
}

func (a *Agent) projectToolsOwned(request toolProjectionRequested) toolProjectionResult {
	a.mu.Lock()
	used := a.budget.outputBudget
	a.mu.Unlock()
	raw := make([]tooling.Result, len(request.results))
	for i, r := range request.results {
		raw[i] = tooling.Result{Output: r.output, IsError: r.isErr, Status: r.status}
	}
	projected, budget := tooling.ApplyOutputBudget(request.calls, raw, int64(request.journal.maxOutputChars), int64(used))
	results := make([]toolRunResult, len(projected))
	for i, result := range projected {
		results[i] = toolRunResult{output: result.Output, isErr: result.IsError, status: result.Status}
	}
	if err := a.persistToolResultsProjected(request.journal, request.calls, results, budget); err != nil {
		return toolProjectionResult{results: results, budget: budget, err: err}
	}
	a.mu.Lock()
	a.budget.outputBudget = int(budget.Used)
	a.mu.Unlock()
	return toolProjectionResult{results: results, budget: budget}
}

func (a *Agent) projectToolsFromWorker(journal toolJournalContext, calls []messages.ToolCall, results []toolRunResult) toolProjectionResult {
	frozenCalls := messages.Clone([]messages.Message{{ToolCalls: calls}})[0].ToolCalls
	journal.statusSink = nil
	request := toolProjectionRequested{journal: journal, calls: frozenCalls, results: append([]toolRunResult(nil), results...), result: make(chan toolProjectionResult, 1)}
	if a.owner == nil {
		return a.projectToolsOwned(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return toolProjectionResult{err: session.ErrClosed}
	}
	select {
	case result := <-request.result:
		return result
	case <-a.owner.done:
		return toolProjectionResult{err: session.ErrClosed}
	}
}
