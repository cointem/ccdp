package agent

import (
	"bytes"
	"context"

	"errors"
	"fmt"

	"os"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

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
