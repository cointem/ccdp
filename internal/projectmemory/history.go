package projectmemory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
)

var ErrHistoryBusy = errors.New("memory history extraction is already running")

// HistoryJob owns only the extraction lock, not the normal write lock. A slow
// model cannot prevent an explicit correction or forget request from saving.
type HistoryJob struct {
	root   *os.Root
	lock   *atomicfile.Lock
	name   string
	Cursor uint64
}

func (s *Store) BeginHistory(ctx context.Context, session string) (*HistoryJob, error) {
	return s.beginHistory(ctx, session, "manual")
}

func (s *Store) BeginAutomaticHistory(ctx context.Context, session string) (*HistoryJob, error) {
	return s.beginHistory(ctx, session, "auto")
}

func (s *Store) beginHistory(ctx context.Context, session, mode string) (*HistoryJob, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if session == "" || session == "." || session == ".." || len(session) > 128 || filepath.Base(session) != session || strings.ContainsAny(session, "/\\\x00\r\n") {
		return nil, errors.New("invalid memory source session")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := s.openRoot(true)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(".history.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		root.Close()
		return nil, errors.New("invalid history lock file")
	}
	lock, err := atomicfile.AcquireOpenedLock(f)
	if err != nil {
		root.Close()
		if errors.Is(err, atomicfile.ErrLocked) {
			return nil, ErrHistoryBusy
		}
		return nil, err
	}
	job := &HistoryJob{root: root, lock: lock, name: ".history-" + mode + "-" + session + ".json"}
	file, err := fsops.OpenRegularAt(root, job.name)
	if os.IsNotExist(err) {
		return job, nil
	}
	if err != nil {
		job.Close()
		return nil, err
	}
	defer file.Close()
	var progress struct {
		Cursor uint64 `json:"cursor"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 256)).Decode(&progress); err != nil {
		job.Close()
		return nil, err
	}
	job.Cursor = progress.Cursor
	return job, nil
}

func (j *HistoryJob) Close() { j.lock.Close(); j.root.Close() }
func (j *HistoryJob) Advance(cursor uint64) error {
	if cursor <= j.Cursor {
		return nil
	}
	raw, err := json.Marshal(struct {
		Cursor uint64 `json:"cursor"`
	}{cursor})
	if err == nil {
		err = write(j.root, j.name, raw)
	}
	if err == nil {
		j.Cursor = cursor
	}
	return err
}

// HistoryNote is a proposed addition (empty ID) or a full replacement of an
// existing note. The model chooses semantic matches; the store validates targets
// and provenance and checks the exact snapshot before applying any proposals.
type HistoryNote struct {
	ID    string `json:"id,omitempty"`
	Turn  string `json:"turn_id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Body  string `json:"body"`
}

// HistorySnapshot includes tombstones, but callers must not send deleted content
// to a model. Reading the snapshot and committing changes use the same lock as
// foreground writes. No lock is held during the model request.
func (s *Store) HistorySnapshot(ctx context.Context) ([]Entry, error) {
	root, err := s.openRoot(false)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	lock, err := acquire(ctx, root)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	return list(root, true)
}

// WithHistorySnapshot guards a second scope while committing project changes.
// Always lock user scope before project scope when both are needed.
func (s *Store) WithHistorySnapshot(ctx context.Context, expected []Entry, fn func() error) error {
	return s.withHistorySnapshot(ctx, expected, func(*os.Root) error { return fn() })
}

func (s *Store) withHistorySnapshot(ctx context.Context, expected []Entry, fn func(*os.Root) error) error {
	root, err := s.openRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := acquire(ctx, root)
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := list(root, true)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(root)
}

// HistoryExcluded blocks old evidence only after a correction or deletion.
// An ordinary saved note does not consume its entire source turn.
func HistoryExcluded(entries []Entry, session, turn string) bool {
	for _, e := range entries {
		sources := e.ExcludedSources
		if e.Deleted {
			sources = append(slices.Clone(sources), e.Sources...)
		}
		for _, src := range sources {
			if src.Session == session && src.Turn == turn {
				return true
			}
		}
	}
	return false
}

// ApplyHistory prevalidates the complete batch, including aggregate storage
// limits. Files are replaced atomically one by one. On an I/O failure the caller
// must leave its cursor unchanged and re-extract against a fresh snapshot; notes
// already saved remain visible for reconciliation and exact duplicates coalesce.
func (s *Store) ApplyHistory(ctx context.Context, expected []Entry, session string, notes []HistoryNote) (int, error) {
	count := 0
	err := s.withHistorySnapshot(ctx, expected, func(root *os.Root) error {
		entries := make(map[string]Entry, len(expected))
		for _, e := range expected {
			e.Sources = slices.Clone(e.Sources)
			e.ExcludedSources = slices.Clone(e.ExcludedSources)
			entries[e.ID] = e
		}
		changed := map[string]bool{}
		targeted := map[string]bool{}
		now := time.Now().UTC()
		for _, note := range notes {
			if session == "" || note.Turn == "" || HistoryExcluded(expected, session, note.Turn) {
				return errors.New("history memory has missing or excluded evidence")
			}
			e := Entry{ID: note.ID, Title: note.Title, Kind: note.Kind, Body: note.Body}
			if e.Kind != "feedback" && e.Kind != "project" && e.Kind != "reference" {
				return errors.New("invalid history memory kind")
			}
			if note.ID != "" {
				old, ok := entries[note.ID]
				if !ok || old.Deleted || targeted[note.ID] {
					return errors.New("invalid or repeated history memory target")
				}
				targeted[note.ID] = true
				e.CreatedAt, e.UpdatedAt, e.Sources, e.ExcludedSources = old.CreatedAt, old.UpdatedAt, old.Sources, old.ExcludedSources
			} else {
				id, err := newNoteID()
				if err != nil {
					return err
				}
				e.ID = id
			}
			if err := normalize(&e); err != nil {
				return err
			}
			// Reuse the original ID for exact duplicates, including earlier proposals
			// from this batch and commits left by an interrupted extraction.
			if note.ID == "" {
				ids := make([]string, 0, len(entries))
				for id := range entries {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				for _, id := range ids {
					old := entries[id]
					if !old.Deleted && old.Kind == e.Kind && old.Body == e.Body {
						e = old
						break
					}
				}
			}
			old, exists := entries[e.ID]
			if exists && old.Body == e.Body && old.Kind == e.Kind && old.Title == e.Title && slices.Contains(old.Sources, Source{Session: session, Turn: note.Turn}) {
				continue
			}
			addSource(&e, Entry{SourceSession: session, SourceTurn: note.Turn})
			if exists && reflect.DeepEqual(old, e) {
				continue
			}
			e.UpdatedAt = now
			if e.CreatedAt.IsZero() {
				e.CreatedAt = now
			}
			entries[e.ID], changed[e.ID] = e, true
		}
		total, active := 0, 0
		encoded := map[string][]byte{}
		for id, e := range entries {
			raw, err := encodeEntry(e)
			if err != nil {
				return err
			}
			if len(raw) > MaxEntryBytes {
				return errors.New("memory entry exceeds size limit")
			}
			if !e.Deleted {
				total += len(raw)
				active++
			}
			if changed[id] {
				encoded[id] = raw
			}
		}
		if total > MaxTotalBytes || active > MaxEntries {
			return errors.New("memory is full; merge or remove obsolete notes")
		}
		ids := make([]string, 0, len(encoded))
		for id := range encoded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := write(root, id+".md", encoded[id]); err != nil {
				return err
			}
			count++
		}
		if count > 0 {
			_ = refreshIndex(root)
		}
		return nil
	})
	return count, err
}
