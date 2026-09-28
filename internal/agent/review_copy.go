package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"ccdp/internal/execution"
	"ccdp/internal/protocol"
)

// reviewBlobReader owns a single Git batch process for the preparation job.
// Pairs are read in priority order; no model or mutable session state is used.
type reviewBlobReader struct {
	process *execution.Process
	input   io.WriteCloser
	output  io.ReadCloser
	reader  *bufio.Reader
}

// One preparation job owns its fixed scope, copy and Git effects. It has no
// access to Agent's mutable configuration, process registry or UI state.
type reviewJob struct {
	copy      *reviewCopy
	workflow  protocol.WorkflowCommand
	git       func(context.Context, ...string) ([]byte, error)
	openBlobs func(context.Context) (*reviewBlobReader, error)
}

func (a *Agent) openReviewBlobs(ctx context.Context, cmd protocol.Command, r *reviewCopy) (*reviewBlobReader, error) {
	argv := []string{"git", "cat-file", "--batch"}
	params := map[string]any{"command": formatExternalCommand(argv)}
	if err := a.precheckCommand("Bash", params); err != nil {
		return nil, err
	}
	var err error
	if authorize, ok := ctx.Value(reviewGitAuthorizationKey{}).(func(context.Context, map[string]any) error); ok {
		err = authorize(ctx, params)
	} else {
		err = a.authorizeCommand(ctx, cmd.ID, "Bash", params)
	}
	if err != nil {
		return nil, err
	}
	argv, _ = execution.ReadOnlyGitArgv(argv)
	p, err := execution.StartArgv(execution.StartRequest{Context: ctx, Argv: argv, Dir: r.root, Sandbox: r.policy, Env: execution.ReadOnlyGitEnvironment(os.Environ())})
	if err != nil {
		return nil, err
	}
	in, err := p.StdinPipe()
	if err != nil {
		execution.CleanupStartedProcess(p)
		return nil, err
	}
	out, err := p.StdoutPipe()
	if err != nil {
		execution.CleanupStartedProcess(p)
		return nil, err
	}
	if err = p.Start(); err != nil {
		return nil, err
	}
	return &reviewBlobReader{p, in, out, bufio.NewReader(out)}, nil
}

func (b *reviewBlobReader) close() {
	_ = b.input.Close()
	_ = b.output.Close()
	_ = b.process.Stop("cancelled")
}

func (b *reviewBlobReader) read(oid string, limit int64) ([]byte, string, error) {
	if _, err := fmt.Fprintln(b.input, oid); err != nil {
		return nil, "", err
	}
	header, err := b.reader.ReadString('\n')
	if err != nil {
		return nil, "", err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != oid || fields[1] != "blob" {
		return nil, "", fmt.Errorf("invalid Git blob response: %q", header)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, "", fmt.Errorf("invalid Git blob size")
	}
	var data []byte
	reason := ""
	if size > limit {
		reason = fmt.Sprintf("file exceeds %d bytes", limit)
		_, err = io.CopyN(io.Discard, b.reader, size)
	} else {
		data = make([]byte, int(size))
		_, err = io.ReadFull(b.reader, data)
	}
	if err != nil {
		return nil, "", err
	}
	sep, err := b.reader.ReadByte()
	if err != nil {
		return nil, "", err
	}
	if sep != '\n' {
		return nil, "", fmt.Errorf("invalid Git blob framing")
	}
	return data, reason, nil
}

func reviewSelected(path string, selections []string) bool {
	if len(selections) == 0 {
		return true
	}
	for _, p := range selections {
		if path == p || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func (r *reviewCopy) exclude(path, reason string) {
	if !r.targets[path] {
		r.omittedContext++
		return
	}
	r.excludedCount++
	if len(r.excluded) < 256 {
		r.excluded[path] = reason
	}
}

func (a *Agent) populateReviewCopy(ctx context.Context, cmd protocol.Command, r *reviewCopy) error {
	j := reviewJob{
		copy: r, workflow: *cmd.Workflow,
		git:       func(ctx context.Context, args ...string) ([]byte, error) { return a.codingGit(ctx, cmd.ID, args...) },
		openBlobs: func(ctx context.Context) (*reviewBlobReader, error) { return a.openReviewBlobs(ctx, cmd, r) },
	}
	return j.prepare(ctx)
}

func (j *reviewJob) prepare(ctx context.Context) error {
	r := j.copy
	r.targets, r.retained = map[string]bool{}, map[string]bool{}
	all := map[string]bool{}
	for p := range r.before {
		all[p] = true
	}
	for p := range r.after {
		all[p] = true
	}
	changed := map[string]bool{}
	if j.workflow.Scope == "uncommitted" && r.baseOID != "" {
		data, err := j.git(ctx, "diff", "--relative", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", r.baseOID, "--")
		if err != nil {
			return err
		}
		for _, p := range strings.Split(string(data), "\x00") {
			if p != "" {
				changed[p] = true
			}
		}
	}
	var targets, contextPaths []string
	for p := range all {
		before, bok := r.before[p]
		after, aok := r.after[p]
		different := !bok || !aok || before.Mode != after.Mode || before.Kind != after.Kind
		if j.workflow.Scope == "uncommitted" {
			different = different || changed[p]
		} else {
			different = different || before.OID != after.OID
		}
		if different && reviewSelected(p, j.workflow.Paths) {
			r.targets[p] = true
			targets = append(targets, p)
		} else {
			contextPaths = append(contextPaths, p)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sort.Strings(targets)
	dirs := map[string]bool{}
	ancestors := map[string]bool{}
	for _, p := range targets {
		d := filepath.Dir(p)
		dirs[d] = true
		for {
			ancestors[d] = true
			if d == "." {
				break
			}
			d = filepath.Dir(d)
		}
	}
	priority := func(p string) int {
		if dirs[filepath.Dir(p)] {
			return 0
		}
		if ancestors[filepath.Dir(p)] {
			switch filepath.Base(p) {
			case "go.mod", "package.json", "Cargo.toml", "Makefile", "tsconfig.json", "pyproject.toml":
				return 1
			}
		}
		return 2
	}
	sort.Slice(contextPaths, func(i, j int) bool {
		a, b := priority(contextPaths[i]), priority(contextPaths[j])
		if a != b {
			return a < b
		}
		return contextPaths[i] < contextPaths[j]
	})
	blobs, err := j.openBlobs(ctx)
	if err != nil {
		return err
	}
	defer blobs.close()
	source, err := os.OpenRoot(r.root)
	if err != nil {
		return err
	}
	defer source.Close()
	read := func(v reviewFile) ([]byte, string, error) {
		if v.Kind == "" {
			return nil, "", nil
		}
		if v.Kind != "regular" {
			return nil, v.Kind + " is not reviewed", nil
		}
		if v.OID != "" {
			return blobs.read(v.OID, r.limits.FileBytes)
		}
		info, e := source.Lstat(v.Path)
		if e != nil {
			return nil, "", e
		}
		if info.Size() > r.limits.FileBytes {
			return nil, fmt.Sprintf("file exceeds %d bytes", r.limits.FileBytes), nil
		}
		data, e := readReviewSource(source, v.Path, info, r.limits.FileBytes)
		if int64(len(data)) > r.limits.FileBytes {
			return nil, "file grew beyond preparation limit", nil
		}
		return data, "", e
	}
	paths := append(targets, contextPaths...)
	workingTree := j.workflow.Scope == "uncommitted"
	autocrlf := false
	if workingTree {
		config, _ := j.git(ctx, "config", "--get", "core.autocrlf")
		value := strings.TrimSpace(string(config))
		autocrlf = value == "true" || value == "input"
	}
	var rules map[string]reviewTextRule
	for index, p := range paths {
		if err = ctx.Err(); err != nil {
			return err
		}
		if len(r.retained) >= r.limits.MaxPaths {
			r.exclude(p, "path quota exhausted")
			continue
		}
		if r.total >= r.limits.TotalBytes {
			r.exclude(p, "byte quota exhausted")
			continue
		}
		if !r.admit(p) {
			continue
		}
		if workingTree {
			if _, loaded := rules[p]; !loaded {
				rules, err = j.textRules(ctx, paths[index:min(index+100, len(paths))], autocrlf)
				if err != nil {
					return err
				}
			}
			if rules[p].exclude != "" {
				r.exclude(p, rules[p].exclude)
				continue
			}
		}
		pair := []reviewFile{r.before[p], r.after[p]}
		var content [2][]byte
		reason := ""
		var size int64
		for i, v := range pair {
			content[i], reason, err = read(v)
			if err != nil {
				return err
			}
			if reason != "" {
				break
			}
			if !utf8.Valid(content[i]) || bytes.IndexByte(content[i], 0) >= 0 || bytes.HasPrefix(content[i], []byte("version https://git-lfs.github.com/spec/v1")) {
				reason = "binary/LFS content is not reviewed"
				break
			}
			if i == 1 && rules[p].normalize {
				content[i] = bytes.ReplaceAll(content[i], []byte("\r\n"), []byte("\n"))
			}
			size += int64(len(content[i]))
		}
		if reason == "" && r.total+size > r.limits.TotalBytes {
			reason = "byte quota exhausted"
		}
		if reason != "" {
			r.exclude(p, reason)
			continue
		}
		for i, side := range []string{"before", "after"} {
			if pair[i].Kind == "" {
				continue
			}
			path := filepath.Join(r.dir, side, p)
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			if err = os.WriteFile(path, content[i], os.FileMode(pair[i].Mode)&0777); err != nil {
				return err
			}
		}
		r.total += size
		r.retained[p] = true
	}
	return nil
}
