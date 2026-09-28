package changes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

type applyStep struct {
	path  string
	after File
}

// applySteps turns a flat logical diff into ordered filesystem operations.
// Directories are structural steps, not snapshot files. Only directories
// obstructing a replacement are removed, and Remove never recursively deletes.
func applySteps(p Plan) ([]applyStep, map[string]File) {
	expected := make(map[string]File)
	removals, writes := []applyStep{}, []applyStep{}
	removeDirs, makeDirs := map[string]bool{}, map[string]bool{}
	for _, edit := range p.Edits {
		expected[edit.Path] = edit.Before
		if edit.After.Kind == "absent" {
			removals = append(removals, applyStep{edit.Path, absent()})
		} else {
			writes = append(writes, applyStep{edit.Path, edit.After})
			for dir := filepath.Dir(edit.Path); dir != "."; dir = filepath.Dir(dir) {
				makeDirs[dir] = true
			}
		}
	}
	for _, write := range writes {
		for _, remove := range removals {
			if strings.HasPrefix(remove.path, write.path+"/") {
				for dir := filepath.Dir(remove.path); ; dir = filepath.Dir(dir) {
					removeDirs[dir] = true
					expected[dir] = File{Kind: "directory"}
					if dir == write.path {
						break
					}
				}
			}
		}
	}
	for dir := range makeDirs {
		if _, ok := expected[dir]; !ok {
			expected[dir] = File{Kind: "container"}
		}
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].path > removals[j].path })
	sort.Slice(writes, func(i, j int) bool { return writes[i].path < writes[j].path })
	dirs := func(set map[string]bool, deepFirst bool) []applyStep {
		paths := make([]string, 0, len(set))
		for path := range set {
			paths = append(paths, path)
		}
		sort.Slice(paths, func(i, j int) bool {
			di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
			if di != dj {
				if deepFirst {
					return di > dj
				}
				return di < dj
			}
			return paths[i] < paths[j]
		})
		out := make([]applyStep, 0, len(paths))
		for _, path := range paths {
			kind := "directory"
			if deepFirst {
				kind = "absent"
			}
			out = append(out, applyStep{path, File{Kind: kind}})
		}
		return out
	}
	steps := append(removals, dirs(removeDirs, true)...)
	steps = append(steps, dirs(makeDirs, false)...)
	return append(steps, writes...), expected
}

func (s *Store) planEntry(root, path string, policy *sandbox.Sandbox) (File, error) {
	if _, err := safePath(root, path); err != nil {
		return File{}, err
	}
	info, err := os.Lstat(filepath.Join(root, path))
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return absent(), nil
	}
	if err != nil {
		return File{}, err
	}
	if info.IsDir() {
		return File{Kind: "directory"}, nil
	}
	return s.ReadFile(root, path, policy)
}

func entryMatches(got, want File) bool {
	if want.Kind == "container" {
		return got.Kind == "absent" || got.Kind == "directory"
	}
	if want.Kind == "directory" {
		return got.Kind == "directory"
	}
	return fileEqual(got, want)
}

func advanceExpected(expected map[string]File, step applyStep) {
	expected[step.path] = step.after
	// A published file replaces the old subtree. Those descendant paths are no
	// longer independently addressable and must not be checked through the file.
	if step.after.Kind == "regular" || step.after.Kind == "symlink" {
		for path := range expected {
			if strings.HasPrefix(path, step.path+"/") {
				delete(expected, path)
			}
		}
	}
}

func (s *Store) validateApply(p Plan, expected map[string]File, steps []applyStep, policy *sandbox.Sandbox) error {
	for path, want := range expected {
		got, err := s.planEntry(p.Root, path, policy)
		if err != nil {
			return err
		}
		if !entryMatches(got, want) {
			return fmt.Errorf("%s: %w", path, ErrStale)
		}
	}
	// Refuse extra worktree contents before deleting any planned leaf. This is
	// especially important when a nonempty directory is to become a file.
	for _, step := range steps {
		if step.after.Kind != "absent" {
			continue
		}
		info, err := os.Lstat(filepath.Join(p.Root, step.path))
		if err != nil || !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(filepath.Join(p.Root, step.path), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(p.Root, path)
			if err != nil {
				return err
			}
			if _, ok := expected[rel]; !ok {
				return fmt.Errorf("unselected path blocks replacement: %s", rel)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Apply(ctx context.Context, p Plan, policy *sandbox.Sandbox) (Journal, error) {
	return s.ApplyWithRecorder(ctx, p, policy, nil)
}

// Journal.NextStep is a cursor in deterministic filesystem steps, not edits.
// A crash after a publication is reconciled against that step's post-state.
func (s *Store) ApplyWithRecorder(ctx context.Context, p Plan, policy *sandbox.Sandbox, before func(string) (func() error, error)) (Journal, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !validID(p.ID) || policy == nil {
		return Journal{}, errors.New("invalid plan or missing path policy")
	}
	check := p
	check.ID = ""
	raw, _ := json.Marshal(check)
	if digest(raw) != p.ID {
		return Journal{}, errors.New("plan content does not match identity")
	}
	root, err := filepath.EvalSymlinks(policy.Workspace)
	if err != nil || root != p.Root {
		return Journal{}, errors.New("plan belongs to another workspace")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return Journal{}, err
	}
	defer handle.Close()
	if s.dir != "" {
		if err = os.MkdirAll(s.dir, 0700); err != nil {
			return Journal{}, err
		}
		lock, e := atomicfile.AcquireLock(filepath.Join(s.dir, "apply.lock"))
		if e != nil {
			return Journal{}, e
		}
		defer lock.Close()
	}
	j, err := s.Journal(p.ID)
	if os.IsNotExist(err) {
		j = Journal{Plan: p, Status: "prepared"}
	} else if err != nil {
		return j, err
	}
	steps, expected := applySteps(p)
	if j.NextStep < 0 || j.NextStep > len(steps) || j.Plan.ID != p.ID {
		return j, errors.New("invalid transaction journal")
	}
	if j.Status == "completed" {
		return j, nil
	}
	for _, edit := range p.Edits {
		if edit.Conflict != "" {
			return j, ErrConflict
		}
	}
	if j.Status == "applying" && j.NextStep < len(steps) {
		step := steps[j.NextStep]
		got, e := s.planEntry(root, step.path, policy)
		if e == nil && entryMatches(got, step.after) {
			j.NextStep++
		}
	}
	for _, step := range steps[:j.NextStep] {
		advanceExpected(expected, step)
	}
	if err = s.validateApply(p, expected, steps[j.NextStep:], policy); err != nil {
		return j, err
	}
	for j.NextStep < len(steps) {
		if err = ctx.Err(); err != nil {
			return j, err
		}
		step := steps[j.NextStep]
		path, e := safePath(root, step.path)
		if e != nil {
			return j, e
		}
		if _, e = policy.ResolveWrite(path); e != nil {
			return j, e
		}
		err = func() error {
			lock, e := fsops.LockPath(path)
			if e != nil {
				return e
			}
			defer lock.Close()
			current, e := s.planEntry(root, step.path, policy)
			if e != nil {
				return e
			}
			if !entryMatches(current, expected[step.path]) {
				return ErrStale
			}
			var finish func() error
			if before != nil && current.Kind != "directory" && step.after.Kind != "directory" {
				finish, e = before(path)
				if e != nil {
					return e
				}
			}
			j.Status, j.Error = "applying", ""
			if e = s.save("transactions", p.ID, j); e != nil {
				return e
			}
			switch step.after.Kind {
			case "directory":
				if current.Kind != "directory" {
					e = handle.Mkdir(step.path, 0755)
				}
			case "absent":
				e = handle.Remove(step.path)
				if os.IsNotExist(e) {
					e = nil
				}
			default:
				var data []byte
				data, e = s.Content(step.after)
				if e == nil {
					e = writeRooted(ctx, handle, step.path, step.after, data)
				}
			}
			if e == nil {
				// Directory entry changes must be durable before advancing the journal.
				parent, openErr := handle.Open(filepath.Dir(step.path))
				if openErr != nil {
					e = openErr
				} else {
					e = errors.Join(parent.Sync(), parent.Close())
				}
			}
			if e == nil && finish != nil {
				e = finish()
			}
			if e != nil {
				j.Error = e.Error()
				_ = s.save("transactions", p.ID, j)
				return e
			}
			advanceExpected(expected, step)
			j.NextStep++
			j.Status = "prepared"
			return s.save("transactions", p.ID, j)
		}()
		if err != nil {
			return j, err
		}
	}
	j.Status = "completed"
	err = s.save("transactions", p.ID, j)
	workspace.Invalidate(root)
	return j, err
}
