package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/execution"
	"ccdp/internal/fsops"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

type reviewFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Mode uint32 `json:"mode"`
	OID  string `json:"-"`
}
type reviewCopy struct {
	limits             config.ReviewConfig
	dir, root          string
	owner              *os.File
	baseOID, targetOID string
	policy             *sandbox.Sandbox
	total              int64
	excluded           map[string]string
	before, after      map[string]reviewFile
	targets            map[string]bool
	retained           map[string]bool
	excludedCount      int
	omittedContext     int
}

func (r *reviewCopy) admit(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." || strings.HasPrefix(path, "../") {
		r.exclude(path, "unsafe path")
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".git" || part == ".ccdp" {
			r.exclude(path, "runtime directory")
			return false
		}
	}
	if _, e := r.policy.ResolveRead(filepath.Join(r.root, path)); e != nil {
		r.exclude(path, "read denied")
		return false
	}
	return true
}

func (a *Agent) prepareReviewCopy(ctx context.Context, cmd protocol.Command) (r *reviewCopy, err error) {
	a.mu.Lock()
	root := a.cfg.Workspace
	limits := a.cfg.Review.Limits()
	policy := a.sandbox.Snapshot()
	a.mu.Unlock()
	if pinned, ok := ctx.Value(reviewSourceKey{}).(reviewSource); ok {
		root, policy = pinned.root, pinned.policy
	}
	cleanAbandonedReviews()
	dir, e := os.MkdirTemp("", "ccdp-review-")
	if e != nil {
		return nil, e
	}
	owner, e := ownReviewDirectory(dir)
	if e != nil {
		os.RemoveAll(dir)
		return nil, e
	}
	r = &reviewCopy{limits: limits, dir: dir, root: root, policy: policy, owner: owner, excluded: map[string]string{}}
	ctx = context.WithValue(ctx, reviewSourceKey{}, reviewSource{root, policy})
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
			owner.Close()
		}
	}()
	for _, side := range []string{"before", "after"} {
		if e = os.Mkdir(filepath.Join(dir, side), 0700); e != nil {
			return nil, e
		}
	}
	w := cmd.Workflow
	head, e := a.resolveReviewRef(ctx, cmd.ID, "HEAD")
	if e != nil {
		if w.Scope != "uncommitted" && w.Scope != "staged" {
			return nil, e
		}
		sym, se := a.codingGit(ctx, cmd.ID, "symbolic-ref", "-q", "HEAD")
		if se != nil {
			return nil, e
		}
		refs, se := a.codingGit(ctx, cmd.ID, "for-each-ref", "--format=%(refname)", strings.TrimSpace(string(sym)))
		if se != nil || len(bytes.TrimSpace(refs)) > 0 {
			return nil, e
		}
		head = ""
	}
	base, target := head, head
	switch w.Scope {
	case "branch":
		ref, e := a.resolveReviewRef(ctx, cmd.ID, w.Base)
		if e != nil {
			return nil, e
		}
		b, e := a.codingGit(ctx, cmd.ID, "merge-base", "--all", ref, head)
		if e != nil {
			return nil, e
		}
		ids := strings.Fields(string(b))
		if len(ids) != 1 {
			return nil, errors.New("ambiguous merge base")
		}
		base = ids[0]
	case "commit":
		target, e = a.resolveReviewRef(ctx, cmd.ID, w.Commit)
		if e != nil {
			return nil, e
		}
		b, e := a.codingGit(ctx, cmd.ID, "rev-list", "--parents", "-n", "1", target)
		if e != nil {
			return nil, e
		}
		parents := strings.Fields(string(b))
		base = ""
		if len(parents) > 2 && w.Parent == 0 {
			return nil, errors.New("merge commit requires --parent")
		}
		parent := max(1, w.Parent)
		if len(parents) > 1 {
			if parent >= len(parents) {
				return nil, errors.New("invalid parent number")
			}
			base = parents[parent]
		}
	case "uncommitted", "staged":
	default:
		return nil, fmt.Errorf("unsupported review scope %q", w.Scope)
	}
	r.baseOID, r.targetOID = base, target
	if w.Scope == "uncommitted" || w.Scope == "staged" {
		r.targetOID = ""
	}
	r.before, e = a.gitEntries(ctx, cmd.ID, base, false)
	if e != nil {
		return nil, e
	}
	if w.Scope != "uncommitted" {
		r.after, e = a.gitEntries(ctx, cmd.ID, target, w.Scope == "staged")
		if e != nil {
			return nil, e
		}
	} else {
		// The index is read once to detect conflicts and obtain tracked modes.
		source, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer source.Close()
		listing, e := a.codingGit(ctx, cmd.ID, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if e != nil {
			return nil, e
		}
		conflicts, e := a.codingGit(ctx, cmd.ID, "ls-files", "-u", "-z")
		if e != nil {
			return nil, e
		}
		if len(conflicts) > 0 {
			return nil, errors.New("resolve index conflicts before review")
		}
		r.after = map[string]reviewFile{}
		for _, path := range strings.Split(string(listing), "\x00") {
			if path == "" {
				continue
			}
			if _, ok := r.after[path]; ok {
				continue
			}

			info, e := source.Lstat(path)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return nil, e
			}
			v := reviewFile{Path: path, Mode: gitFileMode("regular", uint32(info.Mode().Perm())), Kind: "regular"}
			if info.Mode()&os.ModeSymlink != 0 {
				v.Kind, v.Mode = "symlink", 0
			} else if !info.Mode().IsRegular() {
				v.Kind = "nonregular"
			}
			r.after[path] = v
			if e = ctx.Err(); e != nil {
				return nil, e
			}
		}
	}
	if e = a.populateReviewCopy(ctx, cmd, r); e != nil {
		return nil, e
	}
	// Excluded entries are absent on both sides of the textual comparison.
	for path := range r.excluded {
		for _, side := range []string{"before", "after"} {
			_ = os.Remove(filepath.Join(dir, side, path))
		}
	}
	return r, nil
}

func readReviewSource(root *os.Root, path string, expected os.FileInfo, limit int64) ([]byte, error) {
	f, err := fsops.OpenRegularAt(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, fmt.Errorf("review source changed while opening %s", path)
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func (r *reviewCopy) task(ctx context.Context, w *protocol.WorkflowCommand) (string, int, error) {
	selected := func(p string) bool {
		if len(w.Paths) == 0 {
			return true
		}
		for _, s := range w.Paths {
			if p == s || strings.HasPrefix(p, strings.TrimSuffix(s, "/")+"/") {
				return true
			}
		}
		return false
	}
	var paths []string
	all := map[string]bool{}
	for p := range r.before {
		all[p] = true
	}
	for p := range r.after {
		all[p] = true
	}
	for p := range all {
		if selected(p) && r.targets[p] && r.retained[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	type entry struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	var entries []entry
	var patch strings.Builder
	sb := r.policy.Snapshot()
	sb.AddDir(r.dir)
	for _, p := range paths {
		if _, skip := r.excluded[p]; skip {
			continue
		}
		old, oe := os.ReadFile(filepath.Join(r.dir, "before", p))
		next, ne := os.ReadFile(filepath.Join(r.dir, "after", p))
		if oe != nil && !os.IsNotExist(oe) {
			return "", 0, oe
		}
		if ne != nil && !os.IsNotExist(ne) {
			return "", 0, ne
		}
		if oe == nil && ne == nil && bytes.Equal(old, next) && r.before[p].Mode == r.after[p].Mode {
			continue
		}
		status := "modified"
		left, right := filepath.Join("before", p), filepath.Join("after", p)
		if os.IsNotExist(oe) {
			status = "added"
			left = "/dev/null"
		}
		if os.IsNotExist(ne) {
			status = "deleted"
			right = "/dev/null"
		}
		argv, _ := execution.ReadOnlyGitArgv([]string{"git", "diff", "--no-index", "--no-color", "--", left, right})
		res, e := execution.RunArgv(ctx, argv, execution.Request{Dir: r.dir, Sandbox: sb, Env: execution.ReadOnlyGitEnvironment(os.Environ()), OutputLimit: 64 << 20})
		if e != nil {
			return "", 0, e
		}
		if res.TimedOut || res.Truncated || res.ExitCode > 1 {
			return "", 0, fmt.Errorf("diff incomplete: %s", res.Stderr)
		}
		patch.WriteString(res.Stdout)
		if patch.Len() > 128<<20 {
			return "", 0, errors.New("review diff exceeds 128 MiB")
		}
		entries = append(entries, entry{p, status})
	}
	data, e := json.MarshalIndent(struct {
		Changes        []entry           `json:"changes"`
		Excluded       map[string]string `json:"excluded"`
		ExcludedCount  int               `json:"excluded_count"`
		OmittedContext int               `json:"omitted_context_count"`
	}{entries, r.excluded, r.excludedCount, r.omittedContext}, "", "  ")
	if e != nil {
		return "", 0, e
	}
	if e = os.WriteFile(filepath.Join(r.dir, "changes.json"), data, 0600); e != nil {
		return "", 0, e
	}
	if e = os.WriteFile(filepath.Join(r.dir, "changes.patch"), []byte(patch.String()), 0600); e != nil {
		return "", 0, e
	}
	scope := "当前未提交变更"
	switch w.Scope {
	case "staged":
		scope = "暂存区变更"
	case "branch":
		scope = fmt.Sprintf("相对 %s 共同祖先的已提交变更", w.Base)
	case "commit":
		scope = fmt.Sprintf("提交 %s 引入的变更", w.Commit)
	}
	prompt := fmt.Sprintf("请审查%s（%d 个选定文件），按优先级报告值得修复的具体问题。\n\n变更清单：../changes.json\n差异：../changes.patch\n新代码：当前目录\n旧代码：../before/", scope, len(entries))
	if focus := strings.TrimSpace(w.Message); focus != "" {
		prompt += "\n\n关注点：" + focus
	}

	return prompt, len(entries), nil
}

func (a *Agent) runReviewWith(ctx context.Context, cmd protocol.Command, execute reviewExecutor) (output string, outcome protocol.ExecutionOutcome, operationErr error) {
	if cmd.Workflow.Action != "" {
		output, operationErr = a.readReviews(cmd.Workflow)
		return output, protocol.OutcomeSuccess, operationErr
	}
	started := time.Now()
	a.reviewProgress(ctx, cmd.ID, "审查：准备只读副本")
	a.mu.Lock()
	limits := a.cfg.Review.Limits()
	a.mu.Unlock()
	prepCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.PrepareSeconds)*time.Second)
	copy, e := a.prepareReviewCopy(prepCtx, cmd)
	defer cancel()
	if e != nil {
		return "审查准备失败：" + e.Error(), protocol.OutcomeFailed, e
	}
	defer func() {
		if err := os.RemoveAll(copy.dir); err != nil {
			output += "\n\n临时代码清理失败，下次审查会重试回收：" + err.Error()
		}
		_ = copy.owner.Close()
	}()
	prompt, count, e := copy.task(prepCtx, cmd.Workflow)
	if e != nil {
		return "审查准备失败：" + e.Error(), protocol.OutcomeFailed, e
	}
	cancel()
	outcome = protocol.OutcomeSuccess
	defer func() {
		if operationErr != nil {
			outcome = protocol.OutcomeFailed
			if errors.Is(operationErr, context.Canceled) {
				outcome = protocol.OutcomeCancelled
			}
			if output == "" {
				output = "审查未完成：" + operationErr.Error()
			}
		}
		if len(copy.excluded) > 0 {
			if operationErr == nil {
				outcome = protocol.OutcomePartial
			}
			output += fmt.Sprintf("\n\n## 未覆盖项（共 %d 项，最多列出 256 项）\n", copy.excludedCount)
			keys := make([]string, 0, len(copy.excluded))
			for p := range copy.excluded {
				keys = append(keys, p)
			}
			sort.Strings(keys)
			for _, p := range keys {
				output += fmt.Sprintf("- %q: %s\n", p, copy.excluded[p])
			}
		}
		if copy.omittedContext > 0 {
			output += fmt.Sprintf("\n\n辅助上下文有 %d 个文件未复制；不计入选定变更的未覆盖项。", copy.omittedContext)
		}
		output += fmt.Sprintf("\n\n审查范围：%s；用时 %s。位置对应准备期间复制的内容；未执行测试。", cmd.Workflow.Scope, time.Since(started).Round(time.Millisecond))
		row := session.ReviewRecorded{ID: string(cmd.ID), Status: string(outcome), Scope: cmd.Workflow.Scope, BaseOID: copy.baseOID, TargetOID: copy.targetOID, StartedAt: started, FinishedAt: time.Now()}
		state := a.codingState()
		state.reviewMu.Lock()
		if state.pendingReviews == nil {
			state.pendingReviews = map[string]pendingReview{}
		}
		state.pendingReviews[row.ID] = pendingReview{row, output}
		state.reviewMu.Unlock()
		if e := a.saveReview(row.ID); e != nil {
			operationErr = errors.Join(operationErr, &reviewSaveError{cause: e, id: row.ID})
		}

	}()
	if count == 0 {
		if len(copy.excluded) > 0 {
			return "选定范围存在无法检查的文件。", outcome, nil
		}
		return "所选范围无变更。", outcome, nil
	}
	cfg, opts, e := childOptionsFromParent(a, childPurposeReview)
	if e != nil {
		return "", outcome, e
	}
	bindReviewWorkspace(&cfg, &opts, filepath.Join(copy.dir, "after"))
	opts.RootContext = ctx
	a.reviewProgress(ctx, cmd.ID, "审查：检查差异与相关代码")
	output, e = execute(ctx, cfg, opts, prompt)
	if e == nil && strings.TrimSpace(output) == "" {
		e = errors.New("review ended without a final report")
	}
	return output, outcome, e
}
