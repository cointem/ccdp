package agent

import (
	"context"
	"errors"
	"time"

	"ccdp/internal/config"

	"ccdp/internal/protocol"
	"ccdp/internal/supervision"
)

func (s *SessionSupervisor) execute(r *managedRun, ctx context.Context, text string, initial *SessionSnapshot) {
	defer s.directory.WorkerDone()
	defer func() {
		close(r.done)
		s.publishChildOutcome(r)
		s.deliver(r)
	}()
	defer s.directory.Settled(r)
	defer r.workspaceLease.Close()
	defer r.cancel()
	defer r.lease.Close()
	if r.fact.Child.Purpose == childPurposeTask {
		r.parent.runChildHook(ctx, true, text)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r.parent.runChildHook(cleanup, false, r.fact.Child.Run.Output)
		}()
	}
	err := r.parent.childSlotsForRun().acquire(ctx)
	if err == nil {
		defer r.parent.childSlotsForRun().release()
		r.mu.Lock()
		r.fact.Child.Run.Status = "starting"
		r.fact.Child.Run.StartedAt = time.Now().UTC()
		err = s.persist(r, "starting")
		r.mu.Unlock()
	}
	var child supervision.ChildRuntime
	var output string
	if err == nil && r.reviewCommand != nil {
		child, output, err = s.executeReview(r, ctx)
	} else {
		if err == nil {
			var factory supervision.ChildFactory = childFactory{
				config: cloneConfig(&r.cfg), options: r.opts, initial: initial,
				history: r.forkHistory, historySource: r.parent,
				tools: r.toolLease, preHooks: r.preHooks,
				recoveryID: string(r.fact.Child.Run.ID) + "-recovery",
			}
			child, err = factory.Open(ctx)
		}
		// Release the copied prefix even if cancellation prevented construction.
		r.forkHistory = nil
		if err == nil {
			r.mu.Lock()
			r.agent = child
			r.fact.Child.Run.Status = "running"
			err = s.persist(r, "running")
			r.mu.Unlock()
		}
		if err == nil {
			err = s.flushChildMessages(r, child)
			if err == nil {
				output, err = s.drive(r, child, ctx, text)
			}
		}
	}

	if child != nil {
		r.mu.Lock()
		r.fact.Child.Run.Status = "settling"
		r.mu.Unlock()
		finished, finishErr := s.finishChild(r, child, initial, output, err, ctx)
		err = errors.Join(err, finishErr, child.CloseContext(context.Background()))
		r.mu.Lock()
		if finished.Fact.Child.Run.ID != "" {
			finished.Fact.PendingMessages = append([]protocol.InputView(nil), r.fact.PendingMessages...)
			r.fact = finished.Fact
			if finished.Fact.Child.Run.Error != "" {
				r.err = errors.New(finished.Fact.Child.Run.Error)
			}
		}
		r.memory, r.memoryBlobs = finished.Records, finished.Artifacts
		r.mu.Unlock()
		final, _ := child.Snapshot(context.Background())
		r.mu.Lock()
		// The transcript is bounded; retaining History here would keep every
		// completed child runtime's full conversation resident indefinitely.
		final.History = nil
		r.final = final
		if finished.Records == nil {
			r.final = protocol.SessionView{SessionID: final.SessionID, Revision: final.Revision, Settings: final.Settings, SandboxRuntime: final.SandboxRuntime, Usage: final.Usage, ContextUsedTokens: final.ContextUsedTokens, LastTurn: final.LastTurn}
		}
		r.agent = nil
		if failure := child.PersistenceFailure(); failure != nil {
			r.executionBoundaryErr = failure
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	if child == nil {
		setRunOutcome(r, output, err, ctx)
	}
	if err != nil {
		r.err = err
		r.fact.Child.Run.Error = err.Error()
		if r.fact.Child.Run.Status == "succeeded" {
			r.fact.Child.Run.Status = "failed"
		}
	}
	if persistErr := s.recordOutcome(r); persistErr != nil {
		r.executionBoundaryErr = persistErr
		r.err = errors.Join(r.err, persistErr)
		r.fact.Child.Run.Status = "failed"
		r.fact.Child.Run.Error = r.err.Error()
	}
	r.mu.Unlock()
	// Retain only immutable data after settlement. Cold continuation rebinds
	// capabilities explicitly, not by retaining closed providers and transports.
	r.mu.Lock()
	r.initialInput = nil
	r.opts = Options{}
	r.toolLease = nil
	r.preHooks = nil
	r.cfg = config.Config{}
	r.lease = nil
	r.mu.Unlock()
}

func (s *SessionSupervisor) drive(r *managedRun, child supervision.ChildRuntime, ctx context.Context, text string) (string, error) {
	return s.driveInput(r, child, ctx, text, "")
}

func (s *SessionSupervisor) driveInput(r *managedRun, child supervision.ChildRuntime, ctx context.Context, text, suffix string) (string, error) {
	cmd := protocol.NewSubmitInput(protocol.CommandID("run-input-"+string(r.fact.Child.Run.ID)+suffix), protocol.SessionID(child.SessionID()), protocol.InputID("run-input-"+string(r.fact.Child.Run.ID)+suffix), text, protocol.InputFollowup)
	if r.initialInput != nil && suffix == "" {
		cmd.ID = r.initialCommandID
		cmd.Input = r.initialInput
	}
	return supervision.Drive(ctx, child, cmd, childRunBoundary{supervisor: s, run: r, child: child})
}

// childRunBoundary serializes completion with follow-up admission. The driver
// cannot expose idle until this directory reservation changes to settling.
type childRunBoundary struct {
	supervisor *SessionSupervisor
	run        *managedRun
	child      supervision.ChildRuntime
}

func (b childRunBoundary) Publish(update protocol.Update) {
	b.run.mu.Lock()
	row := b.supervisor.childRowLocked(b.run)
	b.run.mu.Unlock()
	b.supervisor.publishChild(row, &update, b.run.parent)
}
func (b childRunBoundary) FinishIfIdle(ctx context.Context, prior protocol.TurnID) (protocol.SessionView, bool, error) {
	b.run.mu.Lock()
	defer b.run.mu.Unlock()
	view, err := b.child.Snapshot(ctx)
	if err != nil {
		return view, false, err
	}
	done := b.run.commandsInFlight == 0 && !view.Busy && view.LastTurn != nil && view.LastTurn.TurnID != prior && (!hasTurnStartingInput(view.PendingInputs) || view.LastTurn.Status == protocol.TurnCancelled || view.LastTurn.Status == protocol.TurnFailed)
	if done {
		b.run.fact.Child.Run.FinishedAt = time.Now().UTC()
		b.run.fact.Child.Run.Status = "settling"
	}
	return view, done, nil
}
