package agent

import (
	"context"
	"encoding/json"
	"errors"

	"io"

	"strings"

	"ccdp/internal/changes"
	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

// deliveryJob captures one plan and its narrow effects. It cannot reach mutable
// Agent configuration, tool state or watcher registries.
type deliveryJob struct {
	id            string
	plan          deliveryPlan
	state         session.DeliveryRecorded
	root          string
	policy        *sandbox.Sandbox
	store         *changes.Store
	originalIndex []byte
	exec          func(context.Context, string, *sandbox.Sandbox, []string, io.Reader, []string) (execution.Result, error)
	git           func(context.Context, ...string) ([]byte, error)
	head          func(context.Context) (string, error)
	stagedTree    func(context.Context) ([]byte, error)
	snapshot      func(context.Context, string) (changes.Snapshot, error)
	record        func(session.DeliveryRecorded) error
	status        func(string, ...any)
}

func (j *deliveryJob) run(ctx context.Context) (output string, err error) {
	defer func() {
		if j.state.Commit != "" && j.state.Stage != "committing" {
			if e := removeDeliveryCandidate(j.state.Candidate); e != nil {
				output += "\nCandidate cleanup deferred: " + e.Error()
			}
		}
	}()
	if j.state.Stage == "completed" {
		b, _ := json.Marshal(j.state)
		return string(b), nil
	}
	if err = j.store.Value(j.plan.OriginalIndex, &j.originalIndex); err != nil {
		return "", err
	}
	if j.state.Stage == "committing" {
		if output, err = j.recoverCommit(ctx); err != nil {
			return output, err
		}
	}
	if j.state.Stage == "prepared" || j.state.Stage == "checking" || j.state.Stage == "checked" {
		if output, err = j.prepareAndCommitCandidate(ctx); err != nil {
			return output, err
		}
	}
	if j.state.Stage == "candidate_committed" || j.state.Stage == "publishing" || j.state.Stage == "recovery_required" {
		if output, err = j.publishLocal(ctx); err != nil {
			return output, err
		}
	}
	return j.publishRemote(ctx)
}

func (j *deliveryJob) recoverCommit(ctx context.Context) (string, error) {
	plan, state, root, policy := j.plan, &j.state, j.root, j.policy
	var e error
	if state.Candidate == "" || state.Tree == "" {
		return "", errors.New("commit recovery record is incomplete")
	}
	if e = validateDeliveryCandidate(state.Candidate); e != nil {
		return "", e
	}
	candidatePolicy := policy.Snapshot()
	candidatePolicy.AddDir(state.Candidate)
	query := func(args ...string) (string, error) {
		r, err := j.exec(ctx, state.Candidate, candidatePolicy, append([]string{"git"}, args...), nil, nil)
		return strings.TrimSpace(r.Stdout), err
	}
	commit, err := query("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	tree, err := query("rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	parents, err := query("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", err
	}
	expectedParents := commit
	if plan.Head != "" {
		expectedParents += " " + plan.Head
	}
	if commit == plan.Head || tree != state.Tree || parents != expectedParents {
		return "candidate retained at " + state.Candidate, errors.New("commit outcome cannot be safely recovered; inspect candidate before preparing again")
	}
	if _, e = j.exec(ctx, root, policy, []string{"git", "-c", "maintenance.auto=false", "fetch", "--no-tags", "--no-write-fetch-head", state.Candidate, commit}, nil, nil); e != nil {
		return "", e
	}
	state.Commit = commit
	state.Stage = "candidate_committed"
	if e = j.record(*state); e != nil {
		return "", e
	}

	return "", nil
}
