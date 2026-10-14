package agent

import (
	"bytes"
	"context"

	"fmt"

	"maps"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/atomicfile"
	"ccdp/internal/changes"
	"ccdp/internal/execution"
)

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
