package agent

import (
	"bytes"
	"ccdp/internal/protocol"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// reviewObjects discovers metadata only; selection and quotas precede blob I/O.
func (a *Agent) gitEntries(ctx context.Context, id protocol.CommandID, ref string, index bool) (map[string]reviewFile, error) {
	files := map[string]reviewFile{}
	if ref == "" && !index {
		return files, nil
	}
	args := []string{"ls-tree", "-rz", ref}
	if index {
		args = []string{"ls-files", "--stage", "-z"}
	}
	listing, err := a.codingGit(ctx, id, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range bytes.Split(listing, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		meta, path, ok := strings.Cut(string(row), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid Git entry")
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, err
		}
		oid := fields[2]
		if index {
			oid = fields[1]
			if fields[2] != "0" {
				return nil, errors.New("resolve index conflicts before review")
			}
		}
		kind := "regular"
		if mode == 0120000 {
			kind = "symlink"
		}
		if mode == 0160000 {
			kind = "submodule"
		}
		files[path] = reviewFile{path, kind, uint32(mode) & 0777, oid}
	}
	return files, nil
}

// gitFileMode is used only for Git comparisons, never local restore snapshots.
// Delivery publishes a candidate and Git index, not these modes to the worktree.
func gitFileMode(kind string, mode uint32) uint32 {
	if kind != "regular" {
		return 0
	}
	if mode&0100 != 0 {
		return 0755
	}
	return 0644
}

// gitWorktreePaths keeps gitlinks out of regular-file capture. Git output here,
// like ls-tree, is workspace-relative. Diff queries must request --relative.
func (a *Agent) gitWorktreePaths(ctx context.Context, cmd protocol.Command, root string) ([]string, error) {
	entries, err := a.gitEntries(ctx, cmd.ID, "", true)
	if err != nil {
		return nil, err
	}
	other, err := a.codingGit(ctx, cmd.ID, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(string(other), "\x00") {
		if p != "" {
			entries[p] = reviewFile{Path: p, Kind: "regular"}
		}
	}
	paths := make([]string, 0, len(entries))
	for path, entry := range entries {
		if entry.Kind == "submodule" {
			for _, selected := range cmd.Workflow.Paths {
				if selected == path {
					return nil, errors.New("submodule paths are not supported: " + path)
				}
			}
			continue
		}
		info, err := os.Lstat(filepath.Join(root, path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, errors.New("tracked file replaced by directory: " + path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
