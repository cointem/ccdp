package agent

import (
	"fmt"
	"strings"

	"ccdp/internal/jsonvalue"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
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
	request := toolStartRequested{call: tc, tool: tool, journal: execution.ctx, started: execution.started, result: make(chan error, 1)}
	var err error

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

	if err == nil {
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
	results []tooling.Result
	result  chan toolProjectionResult
}

func (toolProjectionRequested) ownerControlMessage() {}

type toolProjectionResult struct {
	results []tooling.Result
	budget  session.Budget
	err     error
}

func (a *Agent) projectToolsOwned(request toolProjectionRequested) toolProjectionResult {
	a.mu.Lock()
	used := a.budget.outputBudget
	perResultLimit := a.services.cfg.MaxResultSizeBytes
	a.mu.Unlock()
	if request.journal.config != nil {
		perResultLimit = request.journal.config.MaxResultSizeBytes
	}
	raw := request.results
	a.mu.Lock()
	claimed := map[string]bool{}
	cancelled := a.turn.interruptFlag || a.turn.turnCtx != nil && a.turn.turnCtx.Err() != nil || request.journal.context != nil && request.journal.context.Err() != nil
	for i := range raw {
		r := &raw[i]
		originalProcess := r.Process
		if r.Process != nil {
			if p, ok := a.conversation.durable.Processes[r.Process.ID]; ok && p.State.Released {
				r.Process = protocol.CloneProcessOutput(r.Process)
				r.Process.Log = ""
				if !r.Process.Terminal() {
					ref := p.State.ProcessOutput.Reference()
					r.Process = &ref
				}
			}
		}
		if r.Process != nil && len(r.CompletionIDs) > 0 {
			if p, ok := a.conversation.durable.Processes[r.Process.ID]; ok && (p.Projected || claimed[r.Process.ID] || cancelled) {
				ref := r.Process.Reference()
				r.Process = &ref
				r.CompletionIDs = nil
			}
		}
		if r.Process != nil && len(r.CompletionIDs) > 0 {
			claimed[r.Process.ID] = true
		}
		if originalProcess != nil && originalProcess != r.Process {
			r.Output = strings.Replace(r.Output, originalProcess.Text(), r.Process.Text(), 1)
		}
		if r.Code != nil {
			code := *r.Code
			code.Completed = append([]protocol.ProcessOutput(nil), code.Completed...)
			r.Code = &code
			kept := make([]string, 0, len(code.Processes))
			for _, id := range code.Processes {
				if p, ok := a.conversation.durable.Processes[id]; !ok || !p.State.Released {
					kept = append(kept, id)
				}
			}
			r.Code.Processes = kept
			for i := range r.Code.Completed {
				id := r.Code.Completed[i].ID
				if p, ok := a.conversation.durable.Processes[id]; ok && p.State.Released {
					r.Code.Completed[i].Log = ""
				}
				claimed[id] = true
			}
			if cancelled {
				r.Code.Outputs = nil
				r.CompletionIDs = nil
			}
		}
	}
	a.mu.Unlock()
	results, budget, err := tooling.ApplyOutputBudget(request.calls, raw, perResultLimit, int64(request.journal.maxOutputBytes), int64(used))
	if err != nil {
		// The turn stops before another model request. Reuse the recovery
		// view for completed, unprojected calls so the next explicit input
		// can commit a legal history without rerunning their side effects.
		a.mu.Lock()
		a.conversation.recoveryHistory = a.conversation.durable.Recovered().History
		a.conversation.recoveryBase = len(a.conversation.durable.History)
		a.conversation.recoveryView = nil
		a.mu.Unlock()
		return toolProjectionResult{err: err}
	}
	if err := a.persistToolResultsProjected(request.journal, request.calls, results, budget); err != nil {
		return toolProjectionResult{results: results, budget: budget, err: err}
	}
	for _, result := range results {
		if !result.IsError && result.ObserveOutput != nil {
			result.ObserveOutput(result.Output)
		}
	}
	a.mu.Lock()
	a.budget.outputBudget = int(budget.Used)
	a.mu.Unlock()
	return toolProjectionResult{results: results, budget: budget}
}

func (a *Agent) projectToolsFromWorker(journal toolJournalContext, calls []messages.ToolCall, results []tooling.Result) toolProjectionResult {
	frozenCalls := messages.Clone([]messages.Message{{ToolCalls: calls}})[0].ToolCalls
	request := toolProjectionRequested{journal: journal, calls: frozenCalls, results: append([]tooling.Result(nil), results...), result: make(chan toolProjectionResult, 1)}

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
