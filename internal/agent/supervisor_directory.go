package agent

import (
	"context"

	"sort"

	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/supervision"
)

func (s *SessionSupervisor) lookup(id protocol.SessionID) (*managedRun, error) {
	return s.directory.Lookup(id)
}

func (s *SessionSupervisor) ListChildren(ctx context.Context) ([]protocol.ChildSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.recoveryErr != nil {
		return nil, s.recoveryErr
	}
	runs := s.directory.Children()
	rows := make([]protocol.ChildSession, 0, len(runs))
	for _, r := range runs {
		r.mu.Lock()
		rows = append(rows, s.childRowLocked(r))
		r.mu.Unlock()
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].SessionID < rows[j].SessionID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	return rows, nil
}

// childRowLocked projects a managed run into the enriched directory row the
// frontend consumes: bounded Title/Output, the notify delivery-pending flag,
// live capabilities, and any approval the child is currently blocked on. It is
// the single source of that enrichment, shared by ListChildren and the child
// event stream so a root watcher learns approval/capability changes from events
// rather than polling. Callers must hold r.mu; the child agent's own lock is
// taken internally.
func (s *SessionSupervisor) childRowLocked(r *managedRun) protocol.ChildSession {
	row := r.fact.Child
	row.Title = messages.TruncateUTF8(row.Title, 512)
	row.Run.Output = messages.TruncateUTF8(row.Run.Output, 2048)
	if !row.Run.Active() && r.done != nil {
		select {
		case <-r.done:
		default:
			row.Run.Status = "settling"
			row.Run.FinishedAt = time.Time{}
		}
	}
	row.DeliveryPending = row.Run.WaitPolicy == "notify" && !r.fact.Delivered
	row.Capabilities = protocol.AgentCapabilities{Observe: true, Continue: !row.Run.Active() && isTaskChild(row.Purpose), Cancel: row.Run.Active()}
	if row.Approval != nil && row.Purpose == childPurposeReview && r.agent == nil {
		row.Capabilities.Approve = !r.opts.NonInteractive
	}
	var live supervision.RuntimeStatus
	if r.agent != nil && row.Run.Active() {
		live = r.agent.RunStatus()
	}
	if r.agent != nil && row.Run.Status == "running" {
		row.Capabilities.Send = isTaskChild(row.Purpose)
		row.Capabilities.Interrupt = true
		row.Approval, row.Question, row.Plan = live.Approval, live.Question, live.Plan
		if row.Question != nil {
			row.Run.Status = "waiting_question"
		}
		if row.Plan != nil {
			row.Run.Status = "waiting_plan"
		}
		if row.Approval != nil {
			row.Run.Status = "waiting_approval"
			row.Capabilities.Approve = !r.opts.NonInteractive
		}
	}
	// Fold the child's live usage into the row so a root watcher sees token
	// counts tick during the run instead of only at settlement. Usage() takes the
	// agent lock internally, so this stays outside the pendingApproval critical
	// section above. The baseline subtraction mirrors the cold-recovery path.
	if r.agent != nil && row.Run.Active() {
		u, b := live.Usage, r.fact.UsageBaseline
		row.Run.Usage = protocol.UsageSnapshot{
			InputTokens:  max(0, u.InputTokens-b.InputTokens),
			OutputTokens: max(0, u.OutputTokens-b.OutputTokens),
			CachedTokens: max(0, u.CachedTokens-b.CachedTokens),

			TurnCount: max(0, u.TurnCount-b.TurnCount),
		}
		row.Run.ToolUses = live.ToolUses
	}
	return row
}

func (s *SessionSupervisor) OpenReader(ctx context.Context, id protocol.SessionID) (protocol.SessionClient, error) {
	if id == protocol.SessionID(s.root.SessionID()) {
		return s.root, nil
	}
	r, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	sid := r.fact.Child.SessionID
	runID := r.fact.Child.Run.ID
	r.mu.Unlock()
	return &managedClient{supervisor: s, run: r, id: sid, initialRun: runID}, nil
}
