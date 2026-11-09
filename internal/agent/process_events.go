package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/sessionview"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

type processFactRequested struct {
	fact   protocol.ProcessEvent
	result chan processFactResult
}
type processFactResult struct {
	outputID string
	err      error
}

func (processFactRequested) ownerControlMessage() {}

type processProjectionRequested struct{ result chan error }

func (processProjectionRequested) ownerControlMessage() {}

func (a *Agent) bindProcessEvents(ctx *tools.Context, tc messages.ToolCall, journal toolJournalContext) {
	ctx.ProcessOrigin = sessionview.ToolID(journal.turnID, journal.stepID, tc.ID)
	ctx.ProcessProgress = func(progress protocol.ProcessProgress, output string) {
		a.emit(protocol.EventView{Kind: protocol.EventToolProgress, TurnID: protocol.TurnID(journal.turnID), StepID: protocol.StepID(journal.stepID), Tool: &protocol.ToolView{Progress: &progress, ID: protocol.CallID(tc.ID), Name: tc.Name, Status: "running", Output: output, ParentCallID: protocol.CallID(tc.ParentCallID), ChildIndex: tc.ChildIndex}})
	}
	ctx.ProcessEvent = func(fact protocol.ProcessEvent) (string, error) {
		if fact.Kind == "finished" {
			workspace.Invalidate(ctx.WorkingDir)
		}
		request := processFactRequested{fact: fact, result: make(chan processFactResult, 1)}

		a.ensureRun()
		select {
		case a.owner.control <- request:
		case <-a.owner.done:
			return "", session.ErrClosed
		}
		select {
		case r := <-request.result:
			return r.outputID, r.err
		case <-a.owner.done:
			return "", session.ErrClosed
		}
	}
}

func (a *Agent) commitProcessFact(request processFactRequested) processFactResult {
	if err := a.persistenceFailure(); err != nil {
		return processFactResult{err: err}
	}
	p := a.persistenceHandle()
	if p == nil {
		return processFactResult{err: session.ErrClosed}
	}
	f := request.fact
	a.mu.Lock()
	prior, exists := a.conversation.durable.Processes[f.State.ID]
	a.mu.Unlock()
	if f.Kind == "registered" && exists || f.Kind == "started" && exists && prior.State.Status != "registered" || f.Kind == "finished" && exists && prior.Result != nil || f.Kind == "released" && exists && prior.State.Released {
		return processFactResult{outputID: prior.State.OutputID}
	}
	if f.Kind != "registered" && !exists {
		return processFactResult{err: fmt.Errorf("process fact has no durable registration: %s", f.State.ID)}
	}
	var event session.Event
	switch f.Kind {
	case "registered":
		event = session.ProcessRegistered{State: f.State}
	case "started":
		event = session.ProcessStarted{State: f.State}
	case "finished":
		artifacts, err := p.RequestArtifacts()
		if err != nil {
			return processFactResult{err: a.toolJournalFailure(err)}
		}
		if f.Output == nil {
			// Synthetic recovery outcomes have no live log descriptor.
			f.Output = strings.NewReader(f.State.Output)
		}
		ref, err := artifacts.PutReader(context.Background(), f.Output)
		if err != nil {
			return processFactResult{err: a.toolJournalFailure(err)}
		}
		ref.MediaType = "text/plain; charset=utf-8"
		f.State.OutputID = ref.Hash
		if f.State.Truncated {
			f.State.Warning = fmt.Sprintf("Read retained output with ReadOutput output_id=%q offset=0. %s", ref.Hash, f.State.Warning)
		}
		event = session.ProcessFinished{State: f.State, Result: ref}
	case "released":
		event = session.ProcessReleased{ID: f.State.ID, Revision: f.State.Revision}
	default:
		return processFactResult{err: fmt.Errorf("unknown process fact %q", f.Kind)}
	}
	_, err := p.Commit(session.Batch{TransactionID: "process-" + f.Kind + ":" + f.State.ID, Events: []session.Event{event}})
	if err != nil {
		err = a.toolJournalFailure(err)
	}
	return processFactResult{outputID: f.State.OutputID, err: err}
}

// Recovery reads typed execution facts. It never guesses from tool text and
// never reattaches or restarts a command from an earlier runtime generation.
func (a *Agent) recoverProcesses() error {
	a.mu.Lock()
	state := a.conversation.durable.Clone()
	a.mu.Unlock()
	for _, id := range state.ProcessOrder {
		proc := state.Processes[id]
		if proc.Result == nil {
			p := proc.State
			p.Status = "unknown"
			p.Output = ""
			p.Log = ""
			p.ResultID = "result." + id
			p.Warning = "The previous runtime ended. This handle cannot be reattached; inspect workspace state before starting another command."
			p.FinishedAt = time.Now().UTC()
			p.Revision++
			if r := a.commitProcessFact(processFactRequested{fact: protocol.ProcessEvent{Kind: "finished", State: p}}); r.err != nil {
				return r.err
			}
			proc.State = p
		}
		if !proc.State.Released {
			proc.State.Revision++
			if r := a.commitProcessFact(processFactRequested{fact: protocol.ProcessEvent{Kind: "released", State: proc.State}}); r.err != nil {
				return r.err
			}
		}
	}
	// Refresh the crash view after unknown/released facts are installed.
	a.mu.Lock()
	if a.conversation.recoveryHistory != nil {
		a.conversation.recoveryHistory = a.conversation.durable.Recovered().History
		a.conversation.recoveryBase = len(a.conversation.durable.History)
		a.conversation.recoveryView = nil
	}
	a.mu.Unlock()
	return nil
}

func (a *Agent) projectPendingProcesses() error {
	r := processProjectionRequested{result: make(chan error, 1)}

	a.ensureRun()
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-r.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

func (a *Agent) projectPendingProcessesOwned() error {
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.turn.interruptFlag || a.turn.turnCtx != nil && a.turn.turnCtx.Err() != nil {
		a.mu.Unlock()
		return nil
	}
	pending := a.conversation.durable.PendingProcessResults()
	used, limit, per := a.budget.outputBudget, a.services.cfg.MaxToolOutputBytesPerTurn, a.services.cfg.MaxResultSizeBytes
	a.mu.Unlock()
	fact := session.ProcessResultsProjected{Budget: session.Budget{Used: int64(used), Limit: int64(limit)}}
	for _, p := range pending {
		maxBytes := per
		if maxBytes <= 0 {
			maxBytes = 8192
		}
		const prefix = protocol.ProcessCompletionLabel
		if limit > 0 {
			maxBytes = min(maxBytes, max(0, limit-int(fact.Budget.Used)))
		}
		view, text := protocol.FitProcessOutput(p, maxBytes-len(prefix))
		if text == "" {
			break
		}
		fact.IDs = append(fact.IDs, p.ID)
		fact.Outputs = append(fact.Outputs, view)
		fact.Budget.Used += int64(len(prefix) + len(text))
	}
	if len(fact.IDs) == 0 {
		return nil
	}
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	_, err := p.Commit(session.Batch{TransactionID: messages.StableID("process-projected", fact.IDs), Events: []session.Event{fact}})
	if err != nil {
		return a.toolJournalFailure(err)
	}
	a.mu.Lock()
	a.budget.outputBudget = int(fact.Budget.Used)
	a.mu.Unlock()
	return nil
}

// Check the final, budgeted projection before consuming a completion. Omitted
// metadata leaves the result pending for a later boundary/turn.
func projectedProcessIDs(results []tooling.Result) []string {
	var ids []string
	seen := map[string]bool{}
	for _, r := range results {
		if len(r.CompletionIDs) == 0 {
			continue
		}
		if r.Code != nil {
			for _, p := range r.Code.Completed {
				if p.ID != "" && p.ResultID != "" && p.OutputID != "" && !seen[p.ID] {
					ids = append(ids, p.ID)
					seen[p.ID] = true
				}
			}
		} else if len(r.CompletionIDs) > 0 {
			p := r.Process
			if p != nil && r.Output != "" && p.ResultID != "" && p.OutputID != "" && p.Terminal() && !seen[p.ID] {
				ids = append(ids, p.ID)
				seen[p.ID] = true
			}
		}
	}
	return ids
}
