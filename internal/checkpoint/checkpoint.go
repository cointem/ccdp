// Package checkpoint implements git-based session checkpoints (Claude Code's
// checkpoint idea): a snapshot of the workspace taken at a moment in time,
// restorable with a single git command. Snapshots are created with
// `git stash create` (which produces a commit object without touching the
// working tree) and restored with `git checkout <sha> -- .`.
//
// Checkpoint records are persisted per session under the session directory so
// they survive restarts. The package is a no-op outside a git repository.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record is one persisted checkpoint.
type Record struct {
	ID        string    `json:"id"`
	Summary   string    `json:"summary"`
	SHA       string    `json:"sha"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists checkpoint records for one session.
type Store struct {
	dir string // sessions/checkpoints
}

// NewStore builds a checkpoint store rooted under sessionDir/checkpoints.
func NewStore(sessionDir, sessionID string) *Store {
	return &Store{dir: filepath.Join(sessionDir, "checkpoints", sessionID)}
}

// List returns the stored records, newest first.
func (s *Store) List() []Record {
	data, err := os.ReadFile(filepath.Join(s.dir, "records.json"))
	if err != nil {
		return nil
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.After(recs[j].CreatedAt) })
	return recs
}

// Create snapshots the workspace and records it. Summary is a short label. It
// returns the checkpoint id, or "" when there is nothing to snapshot.
func (s *Store) Create(ws, summary string) (string, error) {
	sha, err := snapshot(ws)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", nil // no changes
	}
	id := fmt.Sprintf("ck-%d", time.Now().UnixNano())
	rec := Record{ID: id, Summary: summary, SHA: sha, CreatedAt: time.Now()}
	recs := append(s.List(), rec)
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })
	if mkErr := os.MkdirAll(s.dir, 0o755); mkErr != nil {
		return "", mkErr
	}
	data, marshalErr := json.MarshalIndent(recs, "", "  ")
	if marshalErr != nil {
		return "", marshalErr
	}
	if writeErr := os.WriteFile(filepath.Join(s.dir, "records.json"), data, 0o600); writeErr != nil {
		return "", writeErr
	}
	return id, nil
}

// Restore rewinds the working tree to a checkpoint's snapshot. It restores all
// tracked files and the index; untracked files added after the checkpoint are
// left in place (a conservative restore).
func (s *Store) Restore(ws, id string) error {
	rec, ok := s.get(id)
	if !ok {
		return fmt.Errorf("checkpoint %q not found", id)
	}
	cmd := exec.Command("git", "checkout", rec.SHA, "--", ".")
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("checkpoint restore failed: %v\n%s", err, out)
	}
	// Reset the index to match the restored tree.
	cmd = exec.Command("git", "reset", "-q", rec.SHA, "--")
	cmd.Dir = ws
	if _, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkpoint reset failed: %v", err)
	}
	return nil
}

// Latest returns the most recent checkpoint record, if any.
func (s *Store) Latest() (Record, bool) {
	recs := s.List()
	if len(recs) == 0 {
		return Record{}, false
	}
	return recs[0], true
}

func (s *Store) get(id string) (Record, bool) {
	for _, r := range s.List() {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// snapshot produces a commit object capturing the current working tree without
// changing it. Returns "" when the tree is clean.
func snapshot(ws string) (string, error) {
	if !isGitRepo(ws) {
		return "", fmt.Errorf("workspace %s is not a git repository", ws)
	}
	cmd := exec.Command("git", "stash", "create", "-u", "ccdp checkpoint")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=ccdp", "GIT_AUTHOR_EMAIL=ccdp@local",
		"GIT_COMMITTER_NAME=ccdp", "GIT_COMMITTER_EMAIL=ccdp@local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git stash create failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// isGitRepo reports whether ws is inside a git repository.
func isGitRepo(ws string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = ws
	return cmd.Run() == nil
}
