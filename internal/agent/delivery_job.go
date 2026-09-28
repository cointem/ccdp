package agent

import (
	"bytes"
	"ccdp/internal/atomicfile"
	"ccdp/internal/changes"
	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
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

func (j *deliveryJob) prepareAndCommitCandidate(ctx context.Context) (string, error) {
	plan, state, root, policy, store := j.plan, &j.state, j.root, j.policy, j.store
	var e error
	if time.Since(plan.CreatedAt) > 30*time.Minute {
		return "", errors.New("delivery plan expired")
	}
	head, e := j.head(ctx)
	if e != nil || head != plan.Head {
		return "", changes.ErrStale
	}
	index, e := os.ReadFile(plan.IndexPath)
	if os.IsNotExist(e) && !plan.IndexExists {
		index = []byte{}
		e = nil
	}
	if e != nil || !bytes.Equal(index, j.originalIndex) {
		return "", changes.ErrStale
	}
	selected, e := store.LoadSnapshot(plan.Selected)
	if e != nil {
		return "", e
	}
	dir := state.Candidate
	if dir == "" {
		dir, e = os.MkdirTemp("", "ccdp-delivery-")
		if e != nil {
			return "", e
		}
		state.Candidate = dir
		if e = j.record(*state); e != nil {
			_ = os.RemoveAll(dir)
			return "", e
		}
	} else if err := validateDeliveryCandidate(dir); err != nil {
		return "", fmt.Errorf("delivery candidate unavailable: %w", err)
	}
	candidatePolicy := sandbox.New(dir)
	candidatePolicy.AllowNetwork = policy.NetworkAllowed()
	candidatePolicy.AddReadOnlyDir(plan.Root)
	run := func(argv ...string) (execution.Result, error) {
		return j.exec(ctx, dir, candidatePolicy, argv, nil, nil)
	}
	if state.Stage == "prepared" {
		if _, e = run("git", "-c", "init.templateDir=", "init", "-q", dir); e != nil {
			return "", e
		}
		if plan.Head != "" {
			if _, e = run("git", "-c", "maintenance.auto=false", "fetch", "--no-tags", "--no-write-fetch-head", plan.Root, plan.Head); e != nil {
				return "", e
			}
			if _, e = run("git", "read-tree", plan.Head); e != nil {
				return "", e
			}
			if _, e = run("git", "update-ref", "--no-deref", "HEAD", plan.Head); e != nil {
				return "", e
			}
		} else {
			if _, e = run("git", "read-tree", "--empty"); e != nil {
				return "", e
			}
		}

		if e = store.Materialize(ctx, selected, dir); e != nil {
			return "", e
		}
		// Preserve repository-defined content conversion and signing semantics.
		localConfig, err := j.git(ctx, "config", "--null", "--get-regexp", `^(filter\..*\.(clean|smudge|required|process)|core\.(autocrlf|eol|attributesfile)|user\.(name|email|signingkey)|commit\.gpgsign|gpg\.(format|program))$`)
		if err == nil {
			for _, entry := range strings.Split(string(localConfig), "\x00") {
				key, value, ok := strings.Cut(entry, "\n")
				if ok {
					if _, e = run("git", "config", key, value); e != nil {
						return "", e
					}
				}
			}
		}
		// Import only selected paths into the private candidate index.
		argv := append([]string{"git", "add", "-A", "--"}, plan.Paths...)
		if _, e = run(argv...); e != nil {
			return "", e
		}
		tree, err := run("git", "write-tree")
		if err != nil {
			return "", err
		}
		state.Tree = strings.TrimSpace(tree.Stdout)
		state.Candidate = dir
		state.Stage = "checking"
		if e = j.record(*state); e != nil {
			return "", e
		}
	}
	if output, err := j.runChecks(ctx, dir, candidatePolicy); err != nil {
		return output, err
	}
	if _, e = run("git", "diff", "--exit-code"); e != nil {
		return "", errors.New("checks changed candidate files; prepare a new selection including those changes")
	}
	hooks, e := j.git(ctx, "rev-parse", "--git-path", "hooks")
	if e != nil {
		return "", e
	}
	hookPath := strings.TrimSpace(string(hooks))
	if configured, err := j.git(ctx, "config", "--path", "--get", "core.hooksPath"); err == nil {
		hookPath = strings.TrimSpace(string(configured))
	}
	if !filepath.IsAbs(hookPath) {
		hookPath = filepath.Join(root, hookPath)
	}
	treeBefore, e := run("git", "write-tree")
	if e != nil {
		return "", e
	}
	if strings.TrimSpace(treeBefore.Stdout) != state.Tree {
		return "", errors.New("checks changed candidate index; prepare a new selection including those changes")
	}
	state.Stage = "committing"
	if e = j.record(*state); e != nil {
		return "", e
	}
	if _, e = run("git", "-c", "core.hooksPath="+hookPath, "commit", "-m", plan.Message); e != nil {
		return "", e
	}
	treeAfter, e := run("git", "rev-parse", "HEAD^{tree}")
	if e != nil {
		return "", e
	}
	if strings.TrimSpace(treeBefore.Stdout) != strings.TrimSpace(treeAfter.Stdout) {
		return "", errors.New("commit hooks changed selected content; candidate not published, prepare again")
	}
	commit, e := run("git", "rev-parse", "HEAD")
	if e != nil {
		return "", e
	}
	state.Commit = strings.TrimSpace(commit.Stdout)
	// Import the immutable candidate objects; no user branch is updated yet.
	if _, e = j.exec(ctx, root, policy, []string{"git", "-c", "maintenance.auto=false", "fetch", "--no-tags", "--no-write-fetch-head", dir, state.Commit}, nil, nil); e != nil {
		return "", e
	}
	state.Stage = "candidate_committed"
	if e = j.record(*state); e != nil {
		return "", e
	}

	return "", nil
}

func (j *deliveryJob) runChecks(ctx context.Context, dir string, candidatePolicy *sandbox.Sandbox) (string, error) {
	plan, state, store := j.plan, &j.state, j.store
	var e error
	if state.Stage == "checked" {
		return "", nil
	}
	type checkResult struct {
		Name     string `json:"name"`
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
		Error    string `json:"error,omitempty"`
	}
	checks := []checkResult{}
	for _, check := range plan.Checks {
		timeout := check.TimeoutSeconds
		if timeout == 0 {
			timeout = 300
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		r, err := j.exec(checkCtx, dir, candidatePolicy, check.Argv, nil, nil)
		cancel()
		j.status("check %s: exit %d", check.Name, r.ExitCode)
		record := checkResult{Name: check.Name, ExitCode: r.ExitCode, Output: r.Output}
		if err != nil {
			record.Error = err.Error()
		}
		checks = append(checks, record)
		state.Checks, e = store.PutValue(checks)
		if e != nil {
			return "", e
		}
		if e = j.record(*state); e != nil {
			return "", e
		}
		if err != nil && !check.Optional {
			return r.Output, fmt.Errorf("required check %s: %w", check.Name, err)
		}
	}
	state.Stage = "checked"
	return "", j.record(*state)

}

func (j *deliveryJob) publishLocal(ctx context.Context) (string, error) {
	plan, state, root, policy, store, id := j.plan, &j.state, j.root, j.policy, j.store, j.id
	var e error
	lock, err := changes.LockWorkspace(j.root)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err = recoverDeliveryIndexLock(plan.IndexPath, id); err != nil {
		return "", err
	}
	var indexSnapshot changes.Snapshot
	originalIndex := j.originalIndex
	indexNow, e := os.ReadFile(plan.IndexPath)
	if os.IsNotExist(e) && !plan.IndexExists {
		indexNow = []byte{}
		e = nil
	}
	if e != nil {
		return "", e
	}
	head, e := j.head(ctx)
	if e != nil {
		return "", e
	}
	currentRef, err := j.git(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(string(currentRef)) != plan.Ref {
		return "", changes.ErrStale
	}
	if head != plan.Head && head != state.Commit {
		return "", changes.ErrStale
	}
	if state.Index != "" {
		var expected []byte
		if e = store.Value(state.Index, &expected); e != nil {
			return "", e
		}
		if head == state.Commit && bytes.Equal(indexNow, expected) {
			state.Stage = "committed"
			if e = j.record(*state); e != nil {
				return "", e
			}
		}
	}
	if state.Stage != "committed" {
		if !bytes.Equal(indexNow, originalIndex) {
			return "", changes.ErrStale
		}
		dir, e := os.MkdirTemp("", "ccdp-index-")
		if e != nil {
			return "", e
		}
		defer os.RemoveAll(dir)
		tmpIndex := filepath.Join(dir, "index")
		if len(originalIndex) > 0 {
			if e = os.WriteFile(tmpIndex, originalIndex, 0600); e != nil {
				return "", e
			}
		}
		sb := policy.Snapshot()
		sb.AddDir(dir)
		env := append(execution.SanitizedEnvironmentFor(execution.EnvironmentGit, os.Environ()), "GIT_INDEX_FILE="+tmpIndex)
		// Rebuild changed index entries against its original snapshot while
		// preserving unrelated staged files and index extension flags.
		stagedTree, e := j.stagedTree(ctx)
		if e != nil {
			return "", e
		}
		staged, e := j.snapshot(ctx, strings.TrimSpace(string(stagedTree)))
		if e != nil {
			return "", e
		}
		baseSnapshot, e := j.snapshot(ctx, plan.Head)
		if e != nil {
			return "", e
		}
		committedSnapshot, e := j.snapshot(ctx, state.Commit)
		if e != nil {
			return "", e
		}
		intended, e := store.Prepare(baseSnapshot, committedSnapshot, staged, nil)
		if e != nil {
			return "", e
		}
		indexSnapshot = staged
		indexSnapshot.Files = maps.Clone(staged.Files)
		for _, edit := range intended.Edits {
			if edit.Conflict != "" {
				return "", fmt.Errorf("normalized candidate conflicts with staged %s", edit.Path)
			}
			if edit.After.Kind == "absent" {
				delete(indexSnapshot.Files, edit.Path)
			} else {
				indexSnapshot.Files[edit.Path] = edit.After
			}
		}
		var indexInput strings.Builder
		for _, edit := range changes.Difference(staged, indexSnapshot) {
			if edit.After.Kind == "absent" {
				indexInput.WriteString("0 " + strings.Repeat("0", max(40, len(plan.Head))) + "\t" + edit.Path + "\x00")
				continue
			}
			b, e := store.Content(edit.After)
			if e != nil {
				return "", e
			}
			r, e := j.exec(ctx, root, sb, []string{"git", "hash-object", "-w", "--stdin", "--no-filters"}, bytesReader(b), env)
			if e != nil {
				return "", e
			}
			mode := "100644"
			if edit.After.Mode&0111 != 0 {
				mode = "100755"
			}
			if edit.After.Kind == "symlink" {
				mode = "120000"
			}
			indexInput.WriteString(mode + " " + strings.TrimSpace(r.Stdout) + "\t" + edit.Path + "\x00")
		}
		if _, e = j.exec(ctx, root, sb, []string{"git", "update-index", "-z", "--index-info"}, strings.NewReader(indexInput.String()), env); e != nil {
			return "", e
		}
		newIndex, e := os.ReadFile(tmpIndex)
		if e != nil {
			return "", e
		}
		state.Index, e = store.PutValue(newIndex)
		if e != nil {
			return "", e
		}
		state.Stage = "publishing"
		if e = j.record(*state); e != nil {
			return "", e
		}
		if _, e = policy.ResolveWrite(plan.IndexPath); e != nil {
			return "", e
		}
		lockPath := plan.IndexPath + ".lock"
		lock, e := createDeliveryIndexLock(plan.IndexPath, id)
		if e != nil {
			return "", fmt.Errorf("index lock held; delivery requires recovery: %w", e)
		}
		defer os.Remove(deliveryIndexOwnerPath(plan.IndexPath, id))
		latestIndex, err := os.ReadFile(plan.IndexPath)
		if os.IsNotExist(err) && !plan.IndexExists {
			latestIndex = []byte{}
			err = nil
		}
		if err != nil || !bytes.Equal(latestIndex, originalIndex) {
			_ = lock.Close()
			_ = os.Remove(lockPath)
			return "", changes.ErrStale
		}
		currentRef, err := j.git(ctx, "symbolic-ref", "-q", "HEAD")
		if err != nil || strings.TrimSpace(string(currentRef)) != plan.Ref {
			_ = lock.Close()
			_ = os.Remove(lockPath)
			return "", changes.ErrStale
		}
		published := false
		defer func() {
			_ = lock.Close()
			if !published {
				_ = os.Remove(lockPath)
			}
		}()
		if _, e = lock.Write(newIndex); e == nil {
			e = lock.Sync()
		}
		if e != nil {
			return "", e
		}
		if e = lock.Close(); e != nil {
			return "", e
		}
		if head == plan.Head {
			original, err := store.LoadSnapshot(plan.Snapshot)
			if err != nil {
				return "", err
			}
			for _, path := range plan.Paths {
				now, err := store.ReadFile(plan.Root, path, policy)
				if err != nil {
					return "", err
				}
				expected, ok := original.Files[path]
				if !ok {
					expected = changes.File{Kind: "absent"}
				}
				if now.Kind != expected.Kind || now.Digest != expected.Digest || now.Mode != expected.Mode {
					return "candidate commit retained; selected files changed during checks", changes.ErrStale
				}
			}
			if _, e = j.exec(ctx, root, policy, []string{"git", "update-ref", "-m", "ccdp delivery " + id, plan.Ref, state.Commit, plan.Head}, nil, nil); e != nil {
				return "", e
			}
		}
		if e = os.Rename(lockPath, plan.IndexPath); e != nil {
			return "commit ref updated; index publication requires recovery", e
		}
		published = true
		if e = atomicfile.SyncDir(filepath.Dir(plan.IndexPath)); e != nil {
			return "index updated; durable completion requires recovery", e
		}
		state.Stage = "committed"
		if e = j.record(*state); e != nil {
			return "", e
		}
	}

	return "", nil
}

func (j *deliveryJob) publishRemote(ctx context.Context) (string, error) {
	plan, state, root, policy := j.plan, &j.state, j.root, j.policy
	var e error
	if plan.Mode == "commit" {
		state.Stage = "completed"
		if e = j.record(*state); e != nil {
			return "", e
		}
		b, _ := json.MarshalIndent(*state, "", "  ")
		return string(b), nil
	}
	if !policy.NetworkAllowed() {
		return fmt.Sprintf("local commit %s retained", state.Commit), errors.New("push/PR requires network capability")
	}
	remoteURL, e := j.git(ctx, "remote", "get-url", "--push", plan.Remote)
	if e != nil {
		return "", e
	}
	if strings.TrimSpace(string(remoteURL)) != plan.RemoteURL {
		return "", errors.New("remote identity changed; stop delivery")
	}
	branch := strings.TrimPrefix(plan.Ref, "refs/heads/")
	if state.Stage == "push_requested" {
		remote, err := j.exec(ctx, root, policy, []string{"git", "ls-remote", "--refs", plan.Remote, plan.Ref}, nil, nil)
		if err != nil {
			return "local commit retained; remote push outcome unknown", err
		}
		fields := strings.Fields(remote.Stdout)
		if len(fields) == 2 && fields[0] == state.Commit && fields[1] == plan.Ref {
			state.Stage = "pushed"
			if e = j.record(*state); e != nil {
				return "", e
			}
		} else {
			// A normal, non-force push can be retried after observing the remote.
			// Git rejects divergence; never reset a remote ref for recovery.
			state.Stage = "committed"
		}
	}
	if state.Stage == "committed" {
		state.Stage = "push_requested"
		if e = j.record(*state); e != nil {
			return "", e
		}
		if _, e = j.exec(ctx, root, policy, []string{"git", "push", plan.Remote, state.Commit + ":" + plan.Ref}, nil, nil); e != nil {
			return "local commit retained: " + state.Commit, e
		}
		state.Stage = "pushed"
		if e = j.record(*state); e != nil {
			return "", e
		}
	}
	if plan.Mode == "pr" && (state.Stage == "pushed" || state.Stage == "pr_requested") {
		repository, e := githubRepository(plan.RemoteURL)
		if e != nil {
			return "commit pushed", e
		}
		find, e := j.exec(ctx, root, policy, []string{"gh", "pr", "list", "--repo", repository, "--head", branch, "--base", plan.Base, "--state", "open", "--json", "url,headRefName,baseRefName"}, nil, nil)
		if e != nil {
			return "commit pushed; PR lookup failed", e
		}
		var prs []struct {
			URL string `json:"url"`
		}
		if e = json.Unmarshal([]byte(find.Stdout), &prs); e != nil {
			return "", e
		}
		if len(prs) > 1 {
			return "", errors.New("multiple matching PRs; select an existing PR explicitly")
		}
		if len(prs) == 1 {
			state.PR = prs[0].URL
		} else {
			if state.Stage == "pr_requested" {
				return "commit pushed; prior PR request outcome is still unknown", errors.New("no matching PR yet; check the remote before retrying")
			}
			dir, e := os.MkdirTemp("", "ccdp-pr-")
			if e != nil {
				return "", e
			}
			defer os.RemoveAll(dir)
			body := filepath.Join(dir, "body.md")
			if e = os.WriteFile(body, []byte(plan.Body), 0600); e != nil {
				return "", e
			}
			sb := policy.Snapshot()
			sb.AddReadOnlyDir(dir)
			argv := []string{"gh", "pr", "create", "--repo", repository, "--head", branch, "--base", plan.Base, "--title", plan.Title, "--body-file", body}
			if plan.Draft {
				argv = append(argv, "--draft")
			}
			state.Stage = "pr_requested"
			if e = j.record(*state); e != nil {
				return "", e
			}
			result, e := j.exec(ctx, root, sb, argv, nil, nil)
			if e != nil {
				return "commit pushed; PR creation uncertain, resume queries existing PRs before retry", e
			}
			state.PR = strings.TrimSpace(result.Stdout)
		}
	}
	state.Stage = "completed"
	if e = j.record(*state); e != nil {
		return "", e
	}
	b, _ := json.MarshalIndent(*state, "", "  ")
	return string(b), nil
}
