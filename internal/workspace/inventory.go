package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Inventory covers tracked files (including hidden/ignored tracked files) and
// visible untracked files. Git's NUL protocol preserves unusual file names.
func Inventory(ctx context.Context, root string, policy *sandbox.Sandbox) ([]string, error) {
	if policy == nil {
		return nil, fmt.Errorf("inventory requires path policy")
	}
	_, isGit, _ := detectGit(root)
	paths := []string{}
	if isGit {
		argv, e := execution.ReadOnlyGitArgv([]string{"git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"})
		if e != nil {
			return nil, e
		}
		r, e := execution.RunArgv(ctx, argv, execution.Request{Dir: root, Sandbox: policy, Env: execution.ReadOnlyGitEnvironment(os.Environ()), Timeout: 30 * time.Second, OutputLimit: 32 << 20})
		if e != nil {
			return nil, e
		}
		if r.ExitCode != 0 || r.Truncated || r.TimedOut {
			return nil, fmt.Errorf("Git inventory incomplete: %s", r.Output)
		}
		paths = strings.Split(r.Stdout, "\x00")
	} else {
		info, e := Scan(root, int(^uint(0)>>1))
		if e != nil {
			return nil, e
		}
		for _, f := range info.Files {
			if !f.IsDir {
				paths = append(paths, f.RelPath)
			}
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, rel := range paths {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
			continue
		}
		control := false
		for _, part := range strings.Split(rel, "/") {
			if part == ".git" || part == ".ccdp" {
				control = true
			}
		}
		if control || seen[rel] {
			continue
		}
		if _, e := policy.ResolveRead(filepath.Join(root, rel)); e != nil {
			continue
		}
		fi, e := os.Lstat(filepath.Join(root, rel))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if fi.IsDir() {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}
