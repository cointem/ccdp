package agent

import (
	"context"
	"errors"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/supervision"
)

type childFinalization struct {
	request supervision.FinishRequest
	digest  string
	result  supervision.FinishResult
}

// FinishRun admits exactly one frozen finalization request before Close starts.
// The independent close lifetime commits the outbox after hooks and before the
// journal closes; callers may abandon their wait without abandoning that work.
func (a *Agent) FinishRun(ctx context.Context, request supervision.FinishRequest) (supervision.FinishResult, error) {
	frozen, digest, err := request.Freeze()
	if err != nil {
		return supervision.FinishResult{}, err
	}
	if frozen.Fact.Child.SessionID != protocol.SessionID(a.SessionID()) || frozen.Fact.Child.Run.ID == "" {
		return supervision.FinishResult{}, errors.New("child finalization identity mismatch")
	}
	a.mu.Lock()
	if a.finalization != nil {
		if a.finalization.digest != digest {
			a.mu.Unlock()
			return supervision.FinishResult{}, errors.New("run finalization already admitted with different data")
		}
	} else {
		if a.lifecycle.closing || a.lifecycle.closed {
			a.mu.Unlock()
			return supervision.FinishResult{}, errors.New("child closed before run finalization was admitted")
		}
		a.finalization = &childFinalization{request: frozen, digest: digest}
	}
	a.mu.Unlock()
	err = a.CloseContext(ctx)
	select {
	case <-a.Done():
		a.mu.Lock()
		result := a.finalization.result
		a.mu.Unlock()
		copied, copyErr := result.Clone()
		return copied, errors.Join(err, copyErr)
	default:
		return supervision.FinishResult{}, err
	}
}

func (a *Agent) finalizeChildOutbox(cleanupErr error) error {
	a.mu.Lock()
	pending := a.finalization
	a.mu.Unlock()
	if pending == nil {
		return nil
	}
	request := pending.request
	fact := request.Fact
	failure := request.Error
	addFailure := func(err error) {
		if err == nil {
			return
		}
		if failure != "" {
			failure += "\n"
		}
		failure += err.Error()
	}
	addFailure(cleanupErr)
	output := request.Output
	if request.Capture != nil {
		a.mu.Lock()
		workspace, policy := a.services.cfg.Workspace, a.services.sandbox
		if policy != nil {
			policy = policy.Snapshot()
		}
		a.mu.Unlock()
		id, err := request.Capture.Capture(context.Background(), workspace, policy)
		addFailure(err)
		if err == nil {
			fact.Child.ResultSnapshotID, fact.Child.MergeStatus = id, "pending_merge"
			output += "\nIsolated changes retained in " + workspace + ". Prepare merge with /merge prepare " + string(fact.Child.SessionID)
		}
	}
	supervision.SetOutcome(&fact, output, failure, request.SaveError, request.Outcome, request.Cancelled, time.Now().UTC())
	done := make(chan error, 1)
	a.owner.control <- childOutboxPrepared{fact: fact, baseline: request.Baseline, result: done}
	return <-done
}

// childOutboxPrepared transfers the frozen external result to the session owner.
// The close worker keeps the writer alive until this acknowledgement arrives.
type childOutboxPrepared struct {
	fact     session.ChildRunRecorded
	baseline protocol.UsageSnapshot
	result   chan error
}

func (childOutboxPrepared) ownerControlMessage() {}

func (a *Agent) commitChildOutbox(prepared childOutboxPrepared) error {
	fact := prepared.fact
	usage := a.Usage()
	baseline := prepared.baseline
	fact.Child.Run.Usage = protocol.UsageSnapshot{
		InputTokens: usage.InputTokens - baseline.InputTokens, OutputTokens: usage.OutputTokens - baseline.OutputTokens,
		CachedTokens: usage.CachedTokens - baseline.CachedTokens, TurnCount: usage.TurnCount - baseline.TurnCount,
	}
	fact.Child.Run.ToolUses = a.ToolUses()
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("child result writer unavailable")
	}
	a.mu.Lock()
	settings, revision, memory := a.sessionSettingsLocked(), a.sequences.settingsRev, a.services.cfg.NoSessionPersistence
	a.mu.Unlock()
	committed, err := p.CommitEvents("run-outbox-"+string(fact.Child.Run.ID), session.SettingsChanged{Revision: revision, Settings: settings}, fact)
	result := supervision.FinishResult{Fact: fact}
	if err == nil {
		result.OutboxCursor = committed.Cursor
	}
	if memory {
		records, artifacts, readErr := p.ReadSnapshot()
		result.Records, result.Artifacts = records, artifacts
		err = errors.Join(err, readErr)
	}
	a.mu.Lock()
	a.finalization.result = result
	a.mu.Unlock()
	return err
}

type workspaceResultCapture struct{ store *changes.Store }

func (c workspaceResultCapture) Capture(ctx context.Context, workspace string, policy *sandbox.Sandbox) (string, error) {
	snapshot, err := c.store.CapturePaths(ctx, workspace, nil, policy)
	return snapshot.ID, err
}

func (s *SessionSupervisor) finishChild(r *managedRun, child supervision.ChildRuntime, initial *SessionSnapshot, output string, executionErr error, executionCtx context.Context) (supervision.FinishResult, error) {
	r.mu.Lock()
	request := supervision.FinishRequest{Fact: r.fact, Output: output, Outcome: r.reviewOutcome, Cancelled: executionCtx.Err() != nil || errors.Is(executionErr, context.Canceled)}
	r.mu.Unlock()
	if executionErr != nil {
		request.Error = executionErr.Error()
	}
	var saveErr *reviewSaveError
	if errors.As(executionErr, &saveErr) {
		request.SaveError = saveErr.Error()
	}
	if initial != nil {
		u := initial.Usage
		request.Baseline = protocol.UsageSnapshot{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, TurnCount: u.TurnCount}
	}
	if request.Fact.Child.BaselineID != "" {
		request.Capture = r.parent.childResultCapture()
	}
	return child.FinishRun(context.Background(), request)
}

var _ supervision.ChildRuntime = (*Agent)(nil)
