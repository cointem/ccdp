package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ccdp/internal/changes"
	"ccdp/internal/config"
	"ccdp/internal/execution"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type ReviewFinding struct {
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Explanation string `json:"explanation"`
	Path        string `json:"path"`
	Side        string `json:"side"`
	Line        int    `json:"line"`
	EndLine     int    `json:"end_line"`
	Evidence    string `json:"evidence"`
}
type ReviewReport struct {
	DurationMs     int64           `json:"duration_ms,omitempty"`
	ID             string          `json:"id"`
	SnapshotID     string          `json:"snapshot_id"`
	BaseSnapshotID string          `json:"base_snapshot_id"`
	Scope          string          `json:"scope"`
	Status         string          `json:"status"`
	Findings       []ReviewFinding `json:"findings"`
	Reviewed       []string        `json:"reviewed"`
	Skipped        []string        `json:"skipped"`
	Summary        string          `json:"summary"`
	Usage          Usage           `json:"usage"`
}

// codingGit preserves stdout bytes (including NUL and trailing newlines) and
// refuses truncated results. Every invocation uses the existing approval gate.
func (a *Agent) codingGit(ctx context.Context, id protocol.CommandID, args ...string) ([]byte, error) {
	argv := append([]string{"git"}, args...)
	parameters := map[string]any{"command": formatExternalCommand(argv)}
	if e := a.precheckCommand("Bash", parameters); e != nil {
		return nil, e
	}
	authorize := func() error { return a.authorizeCommand(ctx, id, "Bash", parameters) }
	if reviewAuthorize, ok := ctx.Value(reviewGitAuthorizationKey{}).(func(context.Context, map[string]any) error); ok {
		authorize = func() error { return reviewAuthorize(ctx, parameters) }
	}
	if e := authorize(); e != nil {
		return nil, e
	}
	a.mu.Lock()
	dir := a.cfg.Workspace
	sb := a.sandbox.Snapshot()
	a.mu.Unlock()
	if source, ok := ctx.Value(reviewSourceKey{}).(reviewSource); ok {
		dir, sb = source.root, source.policy
	}
	argv, e := execution.ReadOnlyGitArgv(argv)
	if e != nil {
		return nil, e
	}
	r, e := execution.RunArgv(ctx, argv, execution.Request{Dir: dir, Sandbox: sb, Env: execution.ReadOnlyGitEnvironment(os.Environ()), OutputLimit: 64 << 20})
	if e != nil {
		return nil, e
	}
	if r.TimedOut {
		return nil, errors.New("Git query timed out")
	}
	if r.Truncated {
		return nil, errors.New("Git query exceeded 64 MiB; narrow the scope")
	}
	if r.ExitCode != 0 {
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(r.Stderr))
	}
	return []byte(r.Stdout), nil
}

func (a *Agent) resolveReviewRef(ctx context.Context, id protocol.CommandID, ref string) (string, error) {
	b, e := a.codingGit(ctx, id, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(b)), e
}

func (a *Agent) gitSnapshot(ctx context.Context, id protocol.CommandID, tree string) (changes.Snapshot, error) {
	store := a.codingState().store
	a.mu.Lock()
	root := a.cfg.Workspace
	policy := a.sandbox.Snapshot()
	a.mu.Unlock()
	out := changes.Snapshot{Root: root, Files: map[string]changes.File{}}
	if tree == "" {
		return store.SaveSnapshot(out)
	}
	entries, e := a.gitEntries(ctx, id, tree, false)
	if e != nil {
		return out, e
	}
	type object struct {
		path, oid string
		mode      uint32
		kind      string
	}
	objects := []object{}
	var input strings.Builder
	for path, entry := range entries {
		if entry.Kind == "submodule" {
			out.Excluded = append(out.Excluded, path+": submodule")
			continue
		}
		if _, e := policy.ResolveRead(filepath.Join(root, path)); e != nil {
			out.Excluded = append(out.Excluded, path+": denied")
			continue
		}
		objects = append(objects, object{path, entry.OID, gitFileMode(entry.Kind, entry.Mode), entry.Kind})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].path < objects[j].path })
	for _, object := range objects {
		input.WriteString(object.oid + "\n")
	}
	if len(objects) == 0 {
		return store.SaveSnapshot(out)
	}
	argv := []string{"git", "cat-file", "--batch"}
	params := map[string]any{"command": formatExternalCommand(argv)}
	if e = a.precheckCommand("Bash", params); e != nil {
		return out, e
	}
	if e = a.authorizeCommand(ctx, id, "Bash", params); e != nil {
		return out, e
	}
	argv, _ = execution.ReadOnlyGitArgv(argv)
	r, e := execution.RunArgv(ctx, argv, execution.Request{Dir: root, Sandbox: policy, Input: strings.NewReader(input.String()), Env: execution.ReadOnlyGitEnvironment(os.Environ()), OutputLimit: 128 << 20})
	if e != nil {
		return out, e
	}
	if r.Truncated || r.TimedOut || r.ExitCode != 0 {
		return out, errors.New("Git snapshot incomplete or over 128 MiB")
	}
	reader := bufio.NewReader(strings.NewReader(r.Stdout))
	for _, o := range objects {
		header, e := reader.ReadString('\n')
		if e != nil {
			return out, e
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != o.oid || fields[1] != "blob" {
			return out, errors.New("invalid Git object response")
		}
		n, e := strconv.Atoi(fields[2])
		if e != nil || n < 0 || n > 128<<20 {
			return out, errors.New("invalid Git blob size")
		}
		data := make([]byte, n)
		if _, e = io.ReadFull(reader, data); e != nil {
			return out, e
		}
		if b, e := reader.ReadByte(); e != nil || b != '\n' {
			return out, errors.New("invalid Git blob framing")
		}
		f, e := store.ImportFile(data, o.mode, o.kind)
		if e != nil {
			return out, e
		}
		out.Files[o.path] = f
	}
	return store.SaveSnapshot(out)
}

func (a *Agent) reviewSnapshots(ctx context.Context, cmd protocol.Command) (changes.Snapshot, changes.Snapshot, error) {
	w := cmd.Workflow
	var left, right changes.Snapshot
	var e error
	head, e := a.resolveReviewRef(ctx, cmd.ID, "HEAD")
	if e != nil {
		if w.Scope != "uncommitted" && w.Scope != "staged" {
			return left, right, e
		}
		// Only an unborn symbolic branch may use an empty base. Permission or
		// repository errors must remain visible.
		symbolic, err := a.codingGit(ctx, cmd.ID, "symbolic-ref", "-q", "HEAD")
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(symbolic)), "refs/heads/") {
			return left, right, e
		}
		refs, err := a.codingGit(ctx, cmd.ID, "for-each-ref", "--format=%(refname)", strings.TrimSpace(string(symbolic)))
		if err != nil || strings.TrimSpace(string(refs)) != "" {
			return left, right, e
		}
		head = ""
	}
	base := head
	target := head
	if w.Scope == "branch" {
		ref, e := a.resolveReviewRef(ctx, cmd.ID, w.Base)
		if e != nil {
			return left, right, e
		}
		b, e := a.codingGit(ctx, cmd.ID, "merge-base", "--all", ref, head)
		if e != nil {
			return left, right, e
		}
		ids := strings.Fields(string(b))
		if len(ids) != 1 {
			return left, right, errors.New("ambiguous or missing merge base")
		}
		base = ids[0]
	}
	if w.Scope == "commit" {
		target, e = a.resolveReviewRef(ctx, cmd.ID, w.Commit)
		if e != nil {
			return left, right, e
		}
		b, e := a.codingGit(ctx, cmd.ID, "rev-list", "--parents", "-n", "1", target)
		if e != nil {
			return left, right, e
		}
		parents := strings.Fields(string(b))
		if len(parents) > 2 {
			return left, right, errors.New("merge commit requires explicit parent comparison")
		}
		base = ""
		if len(parents) == 2 {
			base = parents[1]
		}
	}
	left, e = a.gitSnapshot(ctx, cmd.ID, base)
	if e != nil {
		return left, right, e
	}
	switch w.Scope {
	case "uncommitted":
		a.mu.Lock()
		sb := a.sandbox.Snapshot()
		root := a.cfg.Workspace
		a.mu.Unlock()
		paths, err := a.gitWorktreePaths(ctx, cmd, root)
		if err != nil {
			return left, right, err
		}
		right, e = a.codingState().store.CapturePaths(ctx, root, paths, sb)
		if e == nil {
			for path, file := range right.Files {
				file.Mode = gitFileMode(file.Kind, file.Mode)
				right.Files[path] = file
			}
			right, e = a.codingState().store.SaveSnapshot(right)
		}
	case "staged":
		// write-tree only writes immutable objects; it never changes the index.
		b, err := a.stagedTree(ctx, cmd.ID)
		if err != nil {
			return left, right, err
		}
		right, e = a.gitSnapshot(ctx, cmd.ID, strings.TrimSpace(string(b)))
	default:
		right, e = a.gitSnapshot(ctx, cmd.ID, target)
	}
	return left, right, e
}

type reviewExecutor func(context.Context, config.Config, Options, string) (string, error)

// stagedTree uses a private index because write-tree may refresh cache-tree
// extensions even though it does not change the logical staged entries.
func (a *Agent) stagedTree(ctx context.Context, id protocol.CommandID) ([]byte, error) {
	path, e := a.codingGit(ctx, id, "rev-parse", "--git-path", "index")
	if e != nil {
		return nil, e
	}
	a.mu.Lock()
	root := a.cfg.Workspace
	sb := a.sandbox.Snapshot()
	a.mu.Unlock()
	index := strings.TrimSpace(string(path))
	if !filepath.IsAbs(index) {
		index = filepath.Join(root, index)
	}
	if _, e = sb.ResolveRead(index); e != nil {
		return nil, e
	}
	data, e := os.ReadFile(index)
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	dir, e := os.MkdirTemp("", "ccdp-read-index-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "index")
	if len(data) > 0 {
		if e = os.WriteFile(tmp, data, 0600); e != nil {
			return nil, e
		}
	}
	sb.AddDir(dir)
	env := append(execution.ReadOnlyGitEnvironment(os.Environ()), "GIT_INDEX_FILE="+tmp)
	r, e := a.deliveryExec(ctx, id, root, sb, []string{"git", "write-tree"}, nil, env)
	if e != nil {
		return nil, e
	}
	return []byte(r.Stdout), nil
}

func (a *Agent) readReviews(w *protocol.WorkflowCommand) (string, error) {
	if w.Action == "save" {
		err := a.saveReview(w.ID)
		if err != nil {
			return "", err
		}
		return "审查报告已保存。", nil
	}
	state := a.codingState()
	state.reviewMu.Lock()
	pending, pendingOK := state.pendingReviews[w.ID]
	state.reviewMu.Unlock()
	if w.Action == "report" && pendingOK {
		return pending.text + "\n\n报告尚未保存；可复制正文或执行 /review save " + w.ID, nil
	}

	records, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		return "", e
	}
	rows := []session.ReviewRecorded{}
	for _, record := range records {
		var row session.ReviewRecorded
		switch v := record.Event.(type) {
		case session.ReviewRecorded:
			row = v
		case *session.ReviewRecorded:
			row = *v
		default:
			continue
		}
		rows = append(rows, row)
		if w.Action == "report" && row.ID == w.ID {
			if row.ReportText != "" {
				return row.ReportText, nil
			}
			if row.ReportPath != "" {
				if filepath.Base(row.ReportPath) != row.ReportPath {
					return "", errors.New("invalid report filename")
				}
				a.mu.Lock()
				dir := filepath.Join(a.cfg.SessionDir, a.sessionID, "reviews")
				a.mu.Unlock()
				data, err := os.ReadFile(filepath.Join(dir, row.ReportPath))
				return string(data), err
			}
			var report ReviewReport
			if e = a.codingState().store.Value(row.ReportHash, &report); e != nil {
				return "", e
			}
			b, e := json.MarshalIndent(report, "", "  ")
			return string(b), e
		}
	}
	if w.Action == "report" {
		a.mu.Lock()
		dir := filepath.Join(a.cfg.SessionDir, a.sessionID, "reviews")
		a.mu.Unlock()
		if b, err := os.ReadFile(filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte(w.ID))+".md")); err == nil {
			return "结果已保存，结束记录不完整。\n\n" + string(b), nil
		}
		return "", errors.New("review report not found")
	}
	b, e := json.MarshalIndent(rows, "", "  ")
	return string(b), e
}

// Reviews run independently of the parent's turn; never overwrite its status.
func (a *Agent) reviewProgress(ctx context.Context, id protocol.CommandID, label string) {
	if ctx.Err() != nil {
		return
	}
	a.publishReviewItem(id, protocol.TranscriptItem{ID: nextRuntimeID("stage"), Kind: "system", Text: label, Status: "completed"})
}
