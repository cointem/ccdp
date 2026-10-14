package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

type codingState struct {
	recoveryMu      sync.Mutex
	pendingRecovery func() error
	reviewMu        sync.Mutex
	pendingReviews  map[string]pendingReview
	sessionID       string
	store           *changes.Store
}

type restoreBinding struct {
	Point session.RecoveryPointRecorded `json:"point"`
	Mode  string                        `json:"mode"`
	Plan  changes.Plan                  `json:"plan"`
}

func (a *Agent) codingState() *codingState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.services.coding == nil || a.services.coding.sessionID != a.conversation.sessionID {
		dir := ""
		if !a.services.cfg.NoSessionPersistence {
			dir = filepath.Join(a.services.cfg.SessionDir, a.conversation.sessionID, "coding")
		}
		a.services.coding = &codingState{sessionID: a.conversation.sessionID, store: changes.New(dir)}
	}
	return a.services.coding
}

func (a *Agent) captureCoding(ctx context.Context) (changes.Snapshot, error) {
	a.mu.Lock()
	root := a.services.cfg.Workspace
	sb := a.services.sandbox.Snapshot()
	sessionDir := a.services.cfg.SessionDir
	a.mu.Unlock()
	inventory, e := workspace.Inventory(ctx, root, sb)
	if e != nil {
		return changes.Snapshot{}, e
	}
	paths := []string{}
	for _, relpath := range inventory {
		p := filepath.Join(root, filepath.FromSlash(relpath))
		rel, e := filepath.Rel(sessionDir, p)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		paths = append(paths, relpath)
	}
	return a.codingState().store.CapturePaths(ctx, root, paths, sb)
}

func (a *Agent) captureRecoveryPoint(ctx context.Context, turnID, summary string, excludeMessageID ...string) (session.RecoveryPointRecorded, error) {
	before, e := a.captureCoding(ctx)
	if e != nil {
		return session.RecoveryPointRecorded{}, e
	}
	return a.captureRecoveryContext(before, turnID, summary, excludeMessageID...)
}

func (a *Agent) captureRecoveryContext(before changes.Snapshot, turnID, summary string, excludeMessageID ...string) (session.RecoveryPointRecorded, error) {
	a.mu.Lock()
	history := messages.Clone(a.historyLocked())
	a.mu.Unlock()
	if len(excludeMessageID) > 0 && excludeMessageID[0] != "" {
		for i, m := range history {
			if m.ID == excludeMessageID[0] {
				history = history[:i]
				break
			}
		}
	}
	hash, e := a.codingState().store.PutValue(history)
	if e != nil {
		return session.RecoveryPointRecorded{}, e
	}
	p := session.RecoveryPointRecorded{ID: nextRuntimeID("checkpoint"), TurnID: turnID, Workspace: before.Root, Before: before.ID, History: hash, Summary: truncateRunes(summary, 200), CreatedAt: time.Now().UTC()}
	_, e = a.commitAuxiliary(p.ID+"-captured", p)
	return p, e
}

func (a *Agent) sealRecoveryPoint(ctx context.Context, p session.RecoveryPointRecorded) error {
	after, e := a.captureCoding(ctx)
	if e != nil {
		return e
	}
	p.After = after.ID
	_, e = a.commitAuxiliary(p.ID+"-sealed", p)
	return e
}

func (a *Agent) recoveryPoints() ([]session.RecoveryPointRecorded, error) {
	r, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		return nil, e
	}
	byID := map[string]session.RecoveryPointRecorded{}
	for _, v := range r {
		switch p := v.Event.(type) {
		case session.RecoveryPointRecorded:
			byID[p.ID] = p
		case *session.RecoveryPointRecorded:
			byID[p.ID] = *p
		}
	}
	out := make([]session.RecoveryPointRecorded, 0, len(byID))
	for _, v := range byID {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Capture errors stop protected turns before model tools can change files.
func (a *Agent) beginAutomaticCheckpoint(ctx context.Context, turnID, text, inputID string) (func(), error) {
	if a.children.childState != nil || a.persistenceHandle() == nil {
		return func() {}, nil
	}
	a.mu.Lock()
	full := a.services.cfg.AutomaticCheckpoints != nil && *a.services.cfg.AutomaticCheckpoints
	history := messages.Clone(a.historyLocked())
	root := a.services.cfg.Workspace
	a.mu.Unlock()
	if !full {
		for i, message := range history {
			if message.ID == inputID {
				history = history[:i]
				break
			}
		}
		state := a.codingState()
		state.recoveryMu.Lock()
		state.pendingRecovery = func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			workspace, err := filepath.EvalSymlinks(root)
			if err != nil {
				return err
			}
			hash, err := state.store.PutValue(history)
			if err != nil {
				return err
			}
			point := session.RecoveryPointRecorded{ID: nextRuntimeID("checkpoint"), TurnID: turnID,
				Workspace: workspace, History: hash, Summary: truncateRunes(text, 200), CreatedAt: time.Now().UTC()}
			_, err = a.commitAuxiliary(point.ID+"-captured", point)
			return err
		}
		state.recoveryMu.Unlock()
		return func() {
			state.recoveryMu.Lock()
			state.pendingRecovery = nil
			state.recoveryMu.Unlock()
		}, nil
	}
	p, e := a.captureRecoveryPoint(ctx, turnID, text, inputID)
	if e != nil {
		return nil, e
	}
	return func() {
		if e := a.sealRecoveryPoint(ctx, p); e != nil {
			a.emitStatus("checkpoint incomplete: %v", e)
		}
	}, nil
}

// Optional recovery bookkeeping is independent from file-tool matching.
// File tools already serialize their own writes; no workspace lock is needed.
func (a *Agent) beforeCodingWrite(path string, knownBefore []byte) (func() error, error) {
	return a.recordCodingWriteContent(path, knownBefore)
}

func (a *Agent) recordCodingWrite(path string) (func() error, error) {
	return a.recordCodingWriteContent(path, nil)
}

func (a *Agent) recordCodingWriteContent(path string, knownBefore []byte) (func() error, error) {
	a.mu.Lock()
	root := a.services.cfg.Workspace
	sb := a.services.sandbox.Snapshot()
	turn := fmt.Sprintf("turn-%d", a.sequences.turnSeq)
	a.mu.Unlock()
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	resolved, e := sb.ResolveRead(path)
	if e != nil {
		return nil, e
	}
	parent := filepath.Dir(resolved)
	suffix := filepath.Base(resolved)
	for {
		canonical, err := filepath.EvalSymlinks(parent)
		if err == nil {
			resolved = filepath.Join(canonical, suffix)
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil, err
		}
		suffix = filepath.Join(filepath.Base(parent), suffix)
		parent = next
	}
	rel, e := filepath.Rel(root, resolved)
	if e != nil {
		return nil, e
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return func() error { return nil }, nil
	}

	store := a.codingState().store
	state := a.codingState()
	state.recoveryMu.Lock()
	if state.pendingRecovery != nil {
		if err := state.pendingRecovery(); err != nil {
			state.recoveryMu.Unlock()
			return nil, err
		}
		state.pendingRecovery = nil
	}
	state.recoveryMu.Unlock()
	var before changes.File
	if knownBefore != nil {
		// Edit owns the path lock and already read these bytes. The recorder
		// stores that observation, rather than rereading the same content.
		info, err := os.Lstat(resolved)
		if err != nil {
			return nil, err
		}
		before, e = store.ImportFile(knownBefore, uint32(info.Mode().Perm()), "regular")
	} else {
		before, e = store.ReadFile(root, rel, sb)
	}
	if e != nil {
		return nil, e
	}
	hash, e := store.PutValue(before)
	if e != nil {
		return nil, e
	}
	m := session.FileMutationRecorded{ID: nextRuntimeID("mutation"), Workspace: root, Path: filepath.ToSlash(rel), Before: hash, TurnID: turn, CreatedAt: time.Now().UTC()}
	if _, e = a.commitAuxiliary(m.ID+"-before", m); e != nil {
		return nil, e
	}
	return func() error {
		after, e := store.ReadFile(root, rel, sb)
		if e != nil {
			return e
		}
		m.After, e = store.PutValue(after)
		if e != nil {
			return e
		}
		_, e = a.commitAuxiliary(m.ID+"-after", m)
		workspace.Invalidate(root)
		return e
	}, nil
}
