package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/config"
	"ccdp/internal/execution"
	"ccdp/internal/fsops"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

type deliveryPlan struct {
	Root          string                 `json:"root"`
	Head          string                 `json:"head"`
	Ref           string                 `json:"ref"`
	IndexExists   bool                   `json:"index_exists"`
	IndexPath     string                 `json:"index_path"`
	OriginalIndex string                 `json:"original_index"`
	Snapshot      string                 `json:"snapshot"`
	Selected      string                 `json:"selected"`
	Paths         []string               `json:"paths"`
	Message       string                 `json:"message"`
	Remote        string                 `json:"remote"`
	RemoteURL     string                 `json:"remote_url"`
	Base          string                 `json:"base"`
	Mode          string                 `json:"mode"`
	Title         string                 `json:"title"`
	Body          string                 `json:"body"`
	Draft         bool                   `json:"draft"`
	Checks        []config.DeliveryCheck `json:"checks"`
	CreatedAt     time.Time              `json:"created_at"`
}

func hashBytes(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

func (a *Agent) deliveryExec(ctx context.Context, id protocol.CommandID, dir string, sb *sandbox.Sandbox, argv []string, input io.Reader, env []string) (execution.Result, error) {
	args := map[string]any{"command": formatExternalCommand(argv), "working_directory": dir}
	if e := a.precheckCommand("Bash", args); e != nil {
		return execution.Result{}, e
	}
	if e := a.authorizeCommand(ctx, id, "Bash", args); e != nil {
		return execution.Result{}, e
	}
	if len(env) == 0 {
		purpose := execution.EnvironmentGit
		if argv[0] == "gh" {
			purpose = execution.EnvironmentGitHub
		}
		env = execution.SanitizedEnvironmentFor(purpose, os.Environ())
	}
	r, e := execution.RunArgv(ctx, argv, execution.Request{Dir: dir, Sandbox: sb, Env: env, Input: input, OutputLimit: 4 << 20, Timeout: 5 * time.Minute})
	if e != nil {
		return r, e
	}
	if r.Truncated {
		return r, errors.New("delivery output exceeded limit")
	}
	if r.TimedOut {
		return r, errors.New("delivery command timed out")
	}
	if r.ExitCode != 0 {
		return r, fmt.Errorf("%s exited %d: %s", argv[0], r.ExitCode, strings.TrimSpace(r.Output))
	}
	return r, nil
}

func (a *Agent) deliveryStatus(id string) (session.DeliveryRecorded, error) {
	rows, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		return session.DeliveryRecorded{}, e
	}
	state := session.DeliveryRecorded{PlanID: id, Stage: "prepared"}
	for _, r := range rows {
		switch v := r.Event.(type) {
		case *session.DeliveryRecorded:
			if v.PlanID == id {
				state = *v
			}
		case session.DeliveryRecorded:
			if v.PlanID == id {
				state = v
			}
		}
	}
	return state, nil
}
func (a *Agent) recordDelivery(state session.DeliveryRecorded) error {
	raw, _ := json.Marshal(state)
	_, e := a.persistenceHandle().commitEvents("delivery-"+state.PlanID+"-"+state.Stage+"-"+hashBytes(raw), state)
	return e
}

func (a *Agent) prepareDelivery(ctx context.Context, cmd protocol.Command) (string, error) {
	w := cmd.Workflow
	if w.Action == "inspect" && w.ID != "" {
		var plan deliveryPlan
		if e := a.codingState().store.Value(w.ID, &plan); e != nil {
			return "", e
		}
		status, e := a.deliveryStatus(w.ID)
		if e != nil {
			return "", e
		}
		var checks any
		if status.Checks != "" {
			if e = a.codingState().store.Value(status.Checks, &checks); e != nil {
				return "", e
			}
		}
		b, e := json.MarshalIndent(map[string]any{"plan_id": w.ID, "plan": publicDeliveryPlan(plan), "state": status, "checks": checks}, "", "  ")
		return string(b), e
	}
	review := cmd
	copy := *w
	copy.Scope = "uncommitted"
	copy.Kind = protocol.WorkflowReview
	review.Workflow = &copy
	base, current, e := a.reviewSnapshots(ctx, review)
	if e != nil {
		return "", e
	}
	store := a.codingState().store
	hunks, e := store.Hunks(base, current)
	if e != nil {
		return "", e
	}
	diff := changes.Difference(base, current)
	if w.Action == "inspect" || len(w.Paths) == 0 && len(w.Hunks) == 0 && w.Scope != "staged" && w.Scope != "all" {
		b, _ := json.MarshalIndent(map[string]any{"status": "needs_input", "message": "Select --path, --hunk, --staged or --all explicitly; no files have been staged.", "files": diff, "hunks": hunks}, "", "  ")
		return string(b), nil
	}
	paths := append([]string(nil), w.Paths...)
	target := current
	if w.Scope == "staged" {
		copy.Scope = "staged"
		_, target, e = a.reviewSnapshots(ctx, review)
		if e != nil {
			return "", e
		}
	}
	if w.Scope == "all" || w.Scope == "staged" {
		for _, v := range changes.Difference(base, target) {
			paths = append(paths, v.Path)
		}
	}
	selected, e := store.Select(base, target, paths, w.Hunks)
	if e != nil {
		return "", e
	}
	selectedEdits := changes.Difference(base, selected)
	if len(selectedEdits) == 0 {
		return "", errors.New("nothing selected")
	}
	paths = nil
	for _, v := range selectedEdits {
		paths = append(paths, v.Path)
	}
	head, e := a.deliveryHead(ctx, cmd.ID)
	if e != nil {
		return "", e
	}
	refBytes, e := a.codingGit(ctx, cmd.ID, "symbolic-ref", "-q", "HEAD")
	if e != nil {
		return "", errors.New("detached HEAD: create or select a branch before delivery")
	}
	ref := strings.TrimSpace(string(refBytes))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", errors.New("delivery requires a local branch")
	}
	a.mu.Lock()
	cfg := a.cfg.Clone()
	root := cfg.Workspace
	sb := a.sandbox.Snapshot()
	a.mu.Unlock()
	repoRoot, err := a.codingGit(ctx, cmd.ID, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	canonicalGit, err := filepath.EvalSymlinks(strings.TrimSpace(string(repoRoot)))
	if err != nil || canonicalRoot != canonicalGit {
		return "", errors.New("delivery requires the repository root as workspace")
	}
	if e = cfg.Delivery.Validate(); e != nil {
		return "", e
	}
	indexPathBytes, e := a.codingGit(ctx, cmd.ID, "rev-parse", "--git-path", "index")
	if e != nil {
		return "", e
	}
	indexPath := strings.TrimSpace(string(indexPathBytes))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}
	if _, e = sb.ResolveRead(indexPath); e != nil {
		return "", e
	}
	indexBytes, e := os.ReadFile(indexPath)
	indexExists := e == nil
	if os.IsNotExist(e) {
		indexBytes = []byte{}
		e = nil
	}
	if e != nil {
		return "", e
	}
	oldIndex, e := store.PutValue(indexBytes)
	if e != nil {
		return "", e
	}
	copy.Scope = "staged"
	_, indexSnapshot, e := a.reviewSnapshots(ctx, review)
	if e != nil {
		return "", e
	}
	mergeIndex, e := store.Prepare(base, selected, indexSnapshot, nil)
	if e != nil {
		return "", e
	}
	for _, edit := range mergeIndex.Edits {
		if edit.Conflict != "" {
			return "", fmt.Errorf("selected changes conflict with existing staged intent: %s", edit.Path)
		}
	}
	remote := w.Remote
	if remote == "" {
		remote = cfg.Delivery.Remote
	}
	mode := w.Mode
	if mode == "" {
		mode = "pr"
	}
	if mode != "commit" && mode != "push" && mode != "pr" {
		return "", errors.New("delivery mode must be commit, push or pr")
	}
	remoteURL := ""
	if mode != "commit" {
		if remote == "" {
			b, e := a.codingGit(ctx, cmd.ID, "remote")
			if e != nil {
				return "", e
			}
			rs := strings.Fields(string(b))
			if len(rs) != 1 {
				return "", errors.New("select a remote with --remote")
			}
			remote = rs[0]
		}
		b, e := a.codingGit(ctx, cmd.ID, "remote", "get-url", "--push", remote)
		if e != nil {
			return "", e
		}
		remoteURL = strings.TrimSpace(string(b))
	}
	baseBranch := w.Base
	if baseBranch == "" {
		baseBranch = cfg.Delivery.Base
	}
	if mode == "pr" && baseBranch == "" {
		return "", errors.New("select the PR base branch with --base or delivery.base")
	}
	plan := deliveryPlan{Root: root, Head: head, IndexExists: indexExists, Ref: ref, IndexPath: indexPath, OriginalIndex: oldIndex, Snapshot: current.ID, Selected: selected.ID, Paths: paths, Message: w.Message, Remote: remote, RemoteURL: remoteURL, Base: baseBranch, Mode: mode, Title: w.Title, Body: w.Body, Draft: w.Draft, Checks: cfg.Delivery.Checks, CreatedAt: time.Now().UTC()}
	if plan.Title == "" {
		plan.Title = strings.SplitN(w.Message, "\n", 2)[0]
	}
	id, e := store.PutValue(plan)
	if e != nil {
		return "", e
	}
	if e = a.recordDelivery(session.DeliveryRecorded{PlanID: id, Stage: "prepared"}); e != nil {
		return "", e
	}
	b, e := json.MarshalIndent(map[string]any{"plan_id": id, "plan": publicDeliveryPlan(plan), "changes": selectedEdits, "apply": "/commit-push-pr apply " + id}, "", "  ")
	return string(b), e
}

func (a *Agent) applyDelivery(ctx context.Context, cmd protocol.Command) (string, error) {
	release, reserveErr := a.reserveWorkspaceMutation()
	if reserveErr != nil {
		return "", reserveErr
	}
	defer release()
	id := cmd.Workflow.ID
	store := a.codingState().store
	var plan deliveryPlan
	if err := store.Value(id, &plan); err != nil {
		return "", err
	}
	a.mu.Lock()
	root, policy := a.cfg.Workspace, a.sandbox.Snapshot()
	a.mu.Unlock()
	if plan.Root != root {
		return "", errors.New("delivery belongs to another workspace")
	}
	lease, err := fsops.LockDelivery(root, id)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	state, err := a.deliveryStatus(id)
	if err != nil {
		return "", err
	}
	job := deliveryJob{id: id, plan: plan, state: state, root: root, policy: policy, store: store,
		exec: func(ctx context.Context, dir string, sb *sandbox.Sandbox, argv []string, input io.Reader, env []string) (execution.Result, error) {
			return a.deliveryExec(ctx, cmd.ID, dir, sb, argv, input, env)
		},
		git:        func(ctx context.Context, args ...string) ([]byte, error) { return a.codingGit(ctx, cmd.ID, args...) },
		head:       func(ctx context.Context) (string, error) { return a.deliveryHead(ctx, cmd.ID) },
		stagedTree: func(ctx context.Context) ([]byte, error) { return a.stagedTree(ctx, cmd.ID) },
		snapshot: func(ctx context.Context, ref string) (changes.Snapshot, error) {
			return a.gitSnapshot(ctx, cmd.ID, ref)
		},
		record: a.recordDelivery, status: a.emitStatus,
	}
	return job.run(ctx)
}

func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }
func githubRepository(remote string) (string, error) {
	if strings.HasPrefix(remote, "git@") {
		v := strings.TrimPrefix(remote, "git@")
		host, path, ok := strings.Cut(v, ":")
		if !ok {
			return "", errors.New("invalid Git remote")
		}
		return host + "/" + strings.TrimSuffix(path, ".git"), nil
	}
	u, e := url.Parse(remote)
	if e != nil || u.Host == "" {
		return "", errors.New("PR creation requires an explicit GitHub remote URL")
	}
	return u.Host + "/" + strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), nil
}

func publicDeliveryPlan(plan deliveryPlan) deliveryPlan {
	plan.RemoteURL = sanitizeRemoteURL(plan.RemoteURL)
	return plan
}

func (a *Agent) deliveryHead(ctx context.Context, id protocol.CommandID) (string, error) {
	head, e := a.resolveReviewRef(ctx, id, "HEAD")
	if e == nil {
		return head, nil
	}
	ref, err := a.codingGit(ctx, id, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", e
	}
	refs, err := a.codingGit(ctx, id, "for-each-ref", "--format=%(refname)", strings.TrimSpace(string(ref)))
	if err != nil || strings.TrimSpace(string(refs)) != "" {
		return "", e
	}
	return "", nil
}
