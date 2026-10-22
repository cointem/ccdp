// Package projectmemory stores durable Markdown notes independently of
// conversation history. All mutations share a cross-process lock; reads create nothing.
package projectmemory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/sandbox"
)

const (
	MaxEntries    = 64
	MaxEntryBytes = 8 << 10
	MaxTotalBytes = 256 << 10
	MaxIndexBytes = 4 << 10
)

var (
	ErrConflict = errors.New("memory changed; read the current text before editing")
	ErrReadOnly = errors.New("project memory is read-only in this session")
	validID     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

type Entry struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Body    string   `json:"body,omitempty"`
	Kind    string   `json:"kind"`
	Deleted bool     `json:"deleted,omitempty"`
	Sources []Source `json:"sources,omitempty"`
	// ExcludedSources retains only provenance of superseded evidence, never its text.
	ExcludedSources []Source  `json:"excluded_sources"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	SourceSession   string    `json:"source_session"`
	SourceTurn      string    `json:"source_turn,omitempty"`
	SourceCommand   string    `json:"source_command,omitempty"`
}

// Source identifies evidence without hashing or a user-managed version.
type Source struct {
	Session string `json:"session"`
	Turn    string `json:"turn,omitempty"`
	Command string `json:"command,omitempty"`
}

type Store struct {
	dir      string
	base     string
	relative string
	readOnly bool
}

// Open mirrors the canonical project path below the storage directory without
// creating it. Linked worktrees share their common Git directory; separate
// clones and non-Git workspaces remain independent.
func Open(base, workspace string, readOnly bool) (*Store, error) {
	if base == "" || workspace == "" {
		return nil, errors.New("project memory requires a base directory and workspace")
	}
	base, err := canonicalMissing(base)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	kind, identity := "workspace", canonical
	if common := sandbox.GitCommonDirectory(canonical); common != "" {
		kind, identity = "git", common
	}
	projectPath, err := filepath.Rel(string(filepath.Separator), identity)
	if err != nil || !filepath.IsLocal(projectPath) {
		return nil, fmt.Errorf("project memory requires an absolute local project path: %s", identity)
	}
	relative := filepath.Join(kind, projectPath, "memory")
	return &Store{dir: filepath.Join(base, relative), base: base, relative: relative, readOnly: readOnly}, nil
}

// OpenUser uses the same storage service for preferences shared across projects.
func OpenUser(base string, readOnly bool) (*Store, error) {
	if base == "" {
		return nil, errors.New("memory requires a storage directory")
	}
	base, err := canonicalMissing(base)
	if err != nil {
		return nil, err
	}
	relative := filepath.Join("user", "memory")
	return &Store{base: base, relative: relative, dir: filepath.Join(base, relative), readOnly: readOnly}, nil
}

func (s *Store) Directory() string { return s.dir }

func canonicalMissing(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", os.ErrNotExist
	}
	resolved, err := canonicalMissing(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func (s *Store) openRoot(create bool) (*os.Root, error) {
	if create {
		if s.readOnly {
			return nil, ErrReadOnly
		}
		if err := os.MkdirAll(s.base, 0700); err != nil {
			return nil, err
		}
	}
	base, err := os.OpenRoot(s.base)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	// Check each project-owned component before creation. Root operations
	// cannot follow a racing redirect outside the configured storage root.
	path := ""
	for _, component := range strings.Split(s.relative, string(filepath.Separator)) {
		path = filepath.Join(path, component)
		info, err := base.Lstat(path)
		if create && os.IsNotExist(err) {
			if err = base.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
				return nil, err
			}
			info, err = base.Lstat(path)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("project memory path must contain only directories, without symbolic links")
		}
	}
	return base.OpenRoot(s.relative)
}

func acquire(ctx context.Context, root *os.Root) (*atomicfile.Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("project memory lock is not a regular file")
		}
		lock, err := atomicfile.AcquireOpenedLock(f)
		if !errors.Is(err, atomicfile.ErrLocked) {
			return lock, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func write(root *os.Root, name string, data []byte) error {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".tmp-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(temp, name); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
