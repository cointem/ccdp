package agent

import (
	"context"

	"ccdp/internal/protocol"
)

// Ordinary delegated work belongs to its parent task, not a detached job.
// Capture the runs under the directory lock so cancellation cannot target a
// subsequent follow-up which reuses the same agent identity.
func (s *SessionSupervisor) cancelDelegated(parent *Agent) {
	s.mu.Lock()
	runs := make([]*managedRun, 0, len(s.children))
	for _, run := range s.children {
		if run.parent == parent {
			runs = append(runs, run)
		}
	}
	s.mu.Unlock()
	for _, run := range runs {
		run.mu.Lock()
		if run.fact.Child.Run.WaitPolicy == "notify" && run.fact.Child.Run.Active() && run.cancel != nil {
			run.cancel()
		}
		run.mu.Unlock()
	}
}

// Broadcast generation guarded by Agent.mu. Subscribe before checking state:
// an event arriving during the check closes this generation, so no wake is lost.
func (a *Agent) signalAgentActivityLocked() {
	if a.agentActivity != nil {
		close(a.agentActivity)
	}
	a.agentActivity = make(chan struct{})
}

func (a *Agent) waitAgentEvent(ctx context.Context) (string, error) {
	waitingStatus := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		a.mu.Lock()
		if a.agentActivity == nil {
			a.agentActivity = make(chan struct{})
		}
		changed := a.agentActivity
		for _, input := range a.pendingInputs {
			if input.Strategy == protocol.InputMessage {
				a.mu.Unlock()
				return "collaboration", nil
			}
			if input.Strategy == protocol.InputSteer {
				a.mu.Unlock()
				return "user_input", nil
			}
		}
		a.mu.Unlock()
		if err := a.persistenceFailure(); err != nil {
			return "", err
		}
		active := false
		if a.supervisor != nil {
			rows, err := a.supervisor.ListChildren(ctx)
			if err != nil {
				return "", err
			}
			for _, row := range rows {
				if row.ParentSessionID != protocol.SessionID(a.sessionID) || row.Run.WaitPolicy != "notify" {
					continue
				}
				active = active || row.Run.Active() || row.DeliveryPending
				if row.Approval != nil {
					a.mu.Lock()
					if a.agentAttention == nil {
						a.agentAttention = map[protocol.SessionID]string{}
					}
					fresh := a.agentAttention[row.SessionID] != row.Approval.ID
					a.agentAttention[row.SessionID] = row.Approval.ID
					a.mu.Unlock()
					if fresh {
						return "attention", nil
					}
				}
			}
		}
		if !active {
			// A completion may have arrived after the inbox check.
			select {
			case <-changed:
				continue
			default:
				return "no_pending_work", nil
			}
		}
		if !waitingStatus {
			a.emitStatus("等待子任务消息…")
			waitingStatus = true
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-changed:
		}
	}
}
