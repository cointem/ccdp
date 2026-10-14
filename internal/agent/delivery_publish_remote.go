package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"path/filepath"
	"strings"
)

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
