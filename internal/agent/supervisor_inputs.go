package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/session"
)

// Called after the run has joined its workers but before releasing its writer.
// Cancellation is durable so a later continuation cannot drain old inputs.
func (a *Agent) cancelRunInputs(runID string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	a.ensureTypedPendingLocked()
	var facts []session.Event
	for _, input := range a.pendingInputs {
		facts = append(facts, session.InputCancelled{InputID: string(input.ID), Reason: "run settled"})
	}
	a.mu.Unlock()
	if len(facts) == 0 {
		return nil
	}
	if _, err := a.persistenceHandle().commitEvents("run-cancel-inputs-"+runID, facts...); err != nil {
		return err
	}
	a.mu.Lock()
	a.pendingInputs, a.pendingMsgs = nil, nil
	a.mu.Unlock()
	return nil
}

func childRunOutput(a *Agent, previous map[string]bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A failed continuation must never return a previous run's answer.
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].Role == messages.RoleAssistant && a.history[i].Content != "" && !previous[a.history[i].ID] {
			return a.history[i].Content
		}
	}
	return ""
}
