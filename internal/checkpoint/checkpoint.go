// Package checkpoint implements git-based session checkpoints (Claude Code's
// checkpoint idea): a snapshot of the workspace taken at a moment in time,
// restorable with a single git command. Snapshots are created with
// a plumbing-only tree/commit construction (which never touches the working
// tree) and restored with `git checkout <sha> -- .`.
//
// Checkpoint records are persisted per session under the session directory so
// they survive restarts. The package is a no-op outside a git repository.
package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
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
	mu      sync.RWMutex
	dir     string // sessions/checkpoints
	ctx     context.Context
	sandbox *sandbox.Sandbox
	memory  bool
	records []Record
}

// NewStore builds a checkpoint store rooted under sessionDir/checkpoints.
func NewStore(sessionDir, sessionID string) *Store {
	return &Store{dir: filepath.Join(sessionDir, "checkpoints", sessionID)}
}

// NewMemoryStore keeps checkpoint metadata in the owning Agent only. The git
// snapshot itself remains a git object, but no session/checkpoint records are
// written when --no-session-persistence is selected.
func NewMemoryStore(_ string) *Store {
	return &Store{memory: true}
}

// SetExecutionBoundary binds checkpoint git operations to the owning session
// context and sandbox. It is called during Agent construction before any user
// command can create or restore a checkpoint.
func (s *Store) SetExecutionBoundary(ctx context.Context, sb *sandbox.Sandbox) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ctx, s.sandbox = ctx, sb
	s.mu.Unlock()
}

// executionBoundary returns the latest session-owned execution boundary. The
// returned values are immutable for the duration of a git operation; callers
// that need a shorter lifetime should pass their operation context to
// CreateContext or RestoreContext.
func (s *Store) executionBoundary() (context.Context, *sandbox.Sandbox) {
	if s == nil {
		return context.Background(), nil
	}
	s.mu.RLock()
	ctx, sb := s.ctx, s.sandbox
	s.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, sb
}

// List returns the stored records, newest first.
func (s *Store) List() []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.memory {
		recs := append([]Record(nil), s.records...)
		s.mu.Unlock()
		sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.After(recs[j].CreatedAt) })
		return recs
	}
	recs, err := s.readRecordsLocked()
	s.mu.Unlock()
	if err != nil {
		return nil
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.After(recs[j].CreatedAt) })
	return recs
}

// readRecordsLocked reads the on-disk record list while the store lock is
// held. Keeping the read in the same critical section as appendRecord avoids
// two concurrent checkpoint creates racing through a stale records.json.
func (s *Store) readRecordsLocked() ([]Record, error) {
	path := filepath.Join(s.dir, "records.json")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() > 4<<20 {
		return nil, fmt.Errorf("checkpoint records exceed 4 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// Create snapshots the workspace and records it. Summary is a short label. It
// returns the checkpoint id, or "" when there is nothing to snapshot.
func (s *Store) Create(ws, summary string) (string, error) {
	ctx, sb := s.executionBoundary()
	return s.create(ctx, sb, ws, summary)
}

// CreateContext snapshots the workspace using ctx for the lifetime of this
// operation. In particular, an interrupt cancels a restore/checkpoint while
// it is running instead of waiting for the session root context.
func (s *Store) CreateContext(ctx context.Context, ws, summary string) (string, error) {
	_, sb := s.executionBoundary()
	if ctx == nil {
		ctx = context.Background()
	}
	return s.create(ctx, sb, ws, summary)
}

func (s *Store) create(ctx context.Context, sb *sandbox.Sandbox, ws, summary string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("nil checkpoint store")
	}
	sha, err := s.snapshotContext(ctx, sb, ws)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", nil // no changes
	}
	id := fmt.Sprintf("ck-%d", time.Now().UnixNano())
	rec := Record{ID: id, Summary: summary, SHA: sha, CreatedAt: time.Now()}
	if err := s.appendRecord(rec); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) appendRecord(rec Record) error {
	ctx, _ := s.executionBoundary()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memory {
		s.records = append(s.records, rec)
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	lock, err := acquireRecordLock(ctx, filepath.Join(s.dir, "records.json.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	recs, err := s.readRecordsLocked()
	if err != nil {
		return err
	}
	recs = append(recs, rec)
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(s.dir, "records.json"), data, 0o600)
}

// acquireRecordLock serializes the read/modify/write transaction across
// independent Store values and ccdp processes. atomicfile's flock is
// intentionally non-blocking; a short bounded retry makes concurrent
// checkpoints cooperate without allowing a stuck writer to hang forever.
func acquireRecordLock(ctx context.Context, path string) (*atomicfile.Lock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		lock, err := atomicfile.AcquireLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, atomicfile.ErrLocked) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("checkpoint: timed out waiting for records lock: %w", err)
		case <-ticker.C:
		}
	}
}

// Restore rewinds the working tree to a checkpoint's snapshot. It restores all
// tracked files and the index; untracked files added after the checkpoint are
// left in place (a conservative restore).
func (s *Store) Restore(ws, id string) error {
	ctx, sb := s.executionBoundary()
	return s.restore(ctx, sb, ws, id)
}

// RestoreContext restores a checkpoint using the supplied operation context
// and the current session sandbox. It is the preferred API for typed runtime
// commands; Restore remains as the compatibility wrapper for callers without
// an operation context.
func (s *Store) RestoreContext(ctx context.Context, ws, id string) error {
	_, sb := s.executionBoundary()
	if ctx == nil {
		ctx = context.Background()
	}
	return s.restore(ctx, sb, ws, id)
}

func (s *Store) restore(ctx context.Context, sb *sandbox.Sandbox, ws, id string) error {
	rec, ok := s.get(id)
	if !ok {
		return fmt.Errorf("checkpoint %q not found", id)
	}
	if _, err := runGit(ctx, sb, ws, "checkout", rec.SHA, "--", "."); err != nil {
		return fmt.Errorf("checkpoint restore failed: %w", err)
	}
	// Reset the index to match the restored tree.
	if _, err := runGit(ctx, sb, ws, "reset", "-q", rec.SHA, "--"); err != nil {
		return fmt.Errorf("checkpoint reset failed: %w", err)
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
func (s *Store) snapshot(ws string) (string, error) {
	ctx, sb := s.executionBoundary()
	return s.snapshotContext(ctx, sb, ws)
}

func (s *Store) snapshotContext(ctx context.Context, sb *sandbox.Sandbox, ws string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// `stash create` is deliberately used without -u: that flag is not
	// accepted by this subcommand and is silently ignored by Git. The tracked
	// working tree is captured without changing the user's index or worktree.
	tracked, err := runGit(ctx, sb, ws, "stash", "create", "ccdp checkpoint")
	if err != nil {
		return "", err
	}
	untracked, err := runGit(ctx, sb, ws, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	if untracked == "" {
		return tracked, nil
	}

	// Start with the tracked snapshot (or HEAD for a worktree containing only
	// untracked files), then use a temporary alternate index to ask Git to add
	// the untracked paths. The real index and worktree are never changed.
	baseTree := ""
	if tracked != "" {
		baseTree, err = runGit(ctx, sb, ws, "rev-parse", tracked+"^{tree}")
		if err != nil {
			return "", fmt.Errorf("checkpoint tracked tree: %w", err)
		}
	} else {
		baseTree, _ = runGit(ctx, sb, ws, "rev-parse", "--verify", "HEAD^{tree}")
	}
	gitDir, err := runGit(ctx, sb, ws, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("checkpoint git directory: %w", err)
	}
	indexPath, err := newAlternateIndex(gitDir)
	if err != nil {
		return "", fmt.Errorf("checkpoint temporary index: %w", err)
	}
	defer os.Remove(indexPath)
	indexEnv := []string{"GIT_INDEX_FILE=" + indexPath}
	if baseTree != "" {
		if _, err := runGitWithEnv(ctx, sb, ws, indexEnv, "read-tree", baseTree); err != nil {
			return "", fmt.Errorf("checkpoint initialize tracked tree: %w", err)
		}
	}
	if _, err := runGitWithEnv(ctx, sb, ws, indexEnv, "add", "--all", "--", "."); err != nil {
		return "", fmt.Errorf("checkpoint add untracked files: %w", err)
	}
	tree, err := runGitWithEnv(ctx, sb, ws, indexEnv, "write-tree")
	if err != nil {
		return "", fmt.Errorf("checkpoint write tree: %w", err)
	}
	args := []string{"commit-tree", tree}
	if head, headErr := runGit(ctx, sb, ws, "rev-parse", "--verify", "HEAD"); headErr == nil && head != "" {
		args = append(args, "-p", head)
	}
	args = append(args, "-m", "ccdp checkpoint")
	commit, err := runGit(ctx, sb, ws, args...)
	if err != nil {
		return "", fmt.Errorf("checkpoint commit tree: %w", err)
	}
	return commit, nil
}

func newAlternateIndex(gitDir string) (string, error) {
	f, err := os.CreateTemp(gitDir, ".ccdp-checkpoint-index-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) runGit(ws string, args ...string) (string, error) {
	ctx := context.Background()
	var sb *sandbox.Sandbox
	if s != nil {
		s.mu.RLock()
		if s.ctx != nil {
			ctx = s.ctx
		}
		sb = s.sandbox
		s.mu.RUnlock()
	}
	return runGit(ctx, sb, ws, args...)
}

func runGit(ctx context.Context, sb *sandbox.Sandbox, ws string, args ...string) (string, error) {
	return runGitWithEnv(ctx, sb, ws, nil, args...)
}

func runGitWithEnv(ctx context.Context, sb *sandbox.Sandbox, ws string, extraEnv []string, args ...string) (string, error) {
	if !isGitRepo(ctx, sb, ws) {
		return "", fmt.Errorf("workspace %s is not a git repository", ws)
	}
	env := execution.SanitizedEnvironmentFor(execution.EnvironmentGit, os.Environ())
	env = append(env, "GIT_AUTHOR_NAME=ccdp", "GIT_AUTHOR_EMAIL=ccdp@local", "GIT_COMMITTER_NAME=ccdp", "GIT_COMMITTER_EMAIL=ccdp@local")
	env = append(env, extraEnv...)
	res, err := execution.RunArgv(ctx, append([]string{"git"}, args...), execution.Request{Context: ctx, Dir: ws, Sandbox: sb, Env: env, OutputLimit: 512 * 1024})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git exited %d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	out := strings.TrimSpace(res.Output)
	if strings.HasPrefix(strings.Join(args, " "), "stash create") {
		return out, nil
	}
	return out, nil
}

// isGitRepo reports whether ws is inside a git repository.
func isGitRepo(ctx context.Context, sb *sandbox.Sandbox, ws string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	argv, err := execution.ReadOnlyGitArgv([]string{"git", "rev-parse", "--git-dir"})
	if err != nil {
		return false
	}
	env := execution.ReadOnlyGitEnvironment(execution.SanitizedEnvironmentFor(execution.EnvironmentGit, os.Environ()))
	res, err := execution.RunArgv(ctx, argv, execution.Request{Context: ctx, Dir: ws, Sandbox: sb, Env: env, OutputLimit: 64 * 1024})
	return err == nil && res.ExitCode == 0
}
