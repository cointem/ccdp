package agent

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

	"ccdp/internal/changes"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

type codingState struct {
	reviewMu       sync.Mutex
	pendingReviews map[string]pendingReview
	sessionID      string
	store          *changes.Store
}
type restoreBinding struct {
	Point session.RecoveryPointRecorded `json:"point"`
	Mode  string                        `json:"mode"`
	Plan  changes.Plan                  `json:"plan"`
}

func (a *Agent) codingState() *codingState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.coding == nil || a.coding.sessionID != a.sessionID {
		dir := ""
		if !a.cfg.NoSessionPersistence {
			dir = filepath.Join(a.cfg.SessionDir, a.sessionID, "coding")
		}
		a.coding = &codingState{sessionID: a.sessionID, store: changes.New(dir)}
	}
	return a.coding
}

func (a *Agent) captureCoding(ctx context.Context) (changes.Snapshot, error) {
	a.mu.Lock()
	root := a.cfg.Workspace
	sb := a.sandbox.Snapshot()
	sessionDir := a.cfg.SessionDir
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
	a.mu.Lock()
	history := cloneMessages(a.history)
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
	_, e = a.persistenceHandle().commitEvents(p.ID+"-captured", p)
	return p, e
}

func (a *Agent) sealRecoveryPoint(p session.RecoveryPointRecorded) error {
	after, e := a.captureCoding(a.rootCtx)
	if e != nil {
		return e
	}
	p.After = after.ID
	_, e = a.persistenceHandle().commitEvents(p.ID+"-sealed", p)
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

func (a *Agent) runRestore(ctx context.Context, cmd protocol.Command) (string, error) {
	w := cmd.Workflow
	if w.Action != "list" {
		release, err := a.reserveWorkspaceMutation()
		if err != nil {
			return "", err
		}
		defer release()
	}
	state := a.codingState()
	points, e := a.recoveryPoints()
	if e != nil {
		return "", e
	}
	if w.Action == "list" {
		b, _ := json.MarshalIndent(points, "", "  ")
		return string(b), nil
	}
	if w.Action == "prepare" {
		var point session.RecoveryPointRecorded
		for _, p := range points {
			if p.ID == w.ID {
				point = p
				break
			}
		}
		if point.ID == "" {
			return "", errors.New("recovery point not found")
		}
		a.mu.Lock()
		root := a.cfg.Workspace
		a.mu.Unlock()
		resolved, _ := filepath.EvalSymlinks(root)
		if point.Workspace != resolved {
			return "", errors.New("recovery point belongs to another workspace")
		}
		current, e := a.captureCoding(ctx)
		if e != nil {
			return "", e
		}
		var plan changes.Plan
		if w.Mode == "conversation" {
			plan, e = state.store.Prepare(current, current, current, nil)
		} else if w.Scope == "snapshot" {
			target, err := state.store.LoadSnapshot(point.Before)
			if err != nil {
				return "", err
			}
			plan, e = state.store.Prepare(current, target, current, w.Paths)
		} else {
			plan, e = a.inverseMutationPlan(point, current, w.Paths)
		}
		if e != nil {
			return "", e
		}
		if len(w.Resolutions) > 0 {
			plan, e = state.store.Resolve(plan, w.Resolutions)
			if e != nil {
				return "", e
			}
		}
		binding := restoreBinding{point, w.Mode, plan}
		id, e := state.store.PutValue(binding)
		if e != nil {
			return "", e
		}
		b, _ := json.MarshalIndent(map[string]any{"plan_id": id, "mode": w.Mode, "edits": plan.Edits, "apply": "/rewind apply " + id}, "", "  ")
		return string(b), nil
	}
	if w.Action != "apply" && w.Action != "recover" {
		return "", errors.New("unknown restore action")
	}
	var binding restoreBinding
	if e := state.store.Value(w.ID, &binding); e != nil {
		return "", e
	}
	rows, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		return "", err
	}
	var restoration session.RestoreRecorded
	for _, row := range rows {
		switch v := row.Event.(type) {
		case *session.RestoreRecorded:
			if v.ID == w.ID {
				restoration = *v
			}
		case session.RestoreRecorded:
			if v.ID == w.ID {
				restoration = v
			}
		}
	}
	if restoration.Status == "completed" {
		return "already restored " + binding.Mode + " to " + binding.Point.ID, nil
	}
	if restoration.Status == "" && w.Action == "recover" {
		return "", errors.New("restore has not started; apply the prepared plan first")
	}
	if restoration.Status == "" && time.Since(binding.Plan.CreatedAt) > 30*time.Minute {
		return "", errors.New("restore plan expired; prepare again")
	}
	if a.resources != nil && a.resources.Processes.Count() > 0 {
		return "", errors.New("stop managed background processes before restoring")
	}
	if restoration.Status == "" {
		backup, err := a.captureRecoveryPoint(ctx, "restore-"+w.ID, "Before restoring "+binding.Point.ID)
		if err != nil {
			return "", err
		}
		var h string
		var desired []messages.Message
		if binding.Mode == "conversation" || binding.Mode == "both" {
			if err = state.store.Value(binding.Point.History, &desired); err != nil {
				return "", err
			}
			h, err = state.store.PutValue(desired)
			if err != nil {
				return "", err
			}
		}
		restoration = session.RestoreRecorded{ID: w.ID, Status: "started", History: h, Backup: backup.ID}
		if _, err = a.persistenceHandle().commitEvents("restore-start-"+w.ID, restoration); err != nil {
			return "", err
		}
	}

	if binding.Mode == "conversation" || binding.Mode == "both" {
		if a.supervisor != nil {
			rows, err := a.Sessions().ListChildren(ctx)
			if err != nil {
				return "", err
			}
			for _, row := range rows {
				if row.Run.Active() {
					return "", errors.New("stop active children before restoring conversation")
				}
			}
		}
		if err := a.cancelRunInputs("restore-" + w.ID); err != nil {
			return "", err
		}
	}
	if binding.Mode != "conversation" {
		for _, edit := range binding.Plan.Edits {
			if edit.Conflict != "" {
				return "", changes.ErrConflict
			}
			if e := a.authorizeCommand(ctx, cmd.ID, "Write", map[string]any{"file_path": filepath.Join(binding.Plan.Root, edit.Path), "restore_plan": w.ID}); e != nil {
				return "", e
			}
		}
		a.mu.Lock()
		sb := a.sandbox.Snapshot()
		a.mu.Unlock()
		j, e := state.store.Apply(ctx, binding.Plan, sb)
		if e != nil {
			b, _ := json.Marshal(j)
			return string(b), e
		}
	}
	var history []messages.Message
	if binding.Mode == "code" {
		a.mu.Lock()
		history = cloneMessages(a.history)
		a.mu.Unlock()
		id := "restore-notice-" + w.ID
		found := false
		for _, message := range history {
			if message.ID == id {
				found = true
				break
			}
		}
		if !found {
			history = append(history, messages.Message{ID: id, Role: messages.RoleSystem,
				Content: "Workspace restored using checkpoint " + binding.Point.ID + ". Re-read affected files before further changes.", CreatedAt: binding.Plan.CreatedAt})
		}
	} else if e = state.store.Value(restoration.History, &history); e != nil {
		return "", e
	}
	a.persistMu.Lock()
	e = a.persistHistory(history, "restore-"+w.ID)
	if e == nil {
		a.mu.Lock()
		a.history = history
		a.tokenBaseline.promptTokens = 0
		a.tokenBaseline.historyLen = 0
		a.mu.Unlock()
	}
	a.persistMu.Unlock()
	if e != nil {
		return "files restored; conversation recording requires recover", e
	}
	restoration.Status = "completed"
	if _, e = a.persistenceHandle().commitEvents("restore-complete-"+w.ID, restoration); e != nil {
		return "files restored; completion requires recover", e
	}

	if a.resources != nil {
		a.resources.Files.Clear()
	}
	workspace.Invalidate(binding.Plan.Root)
	a.emit(Event{Type: EventHistoryChanged, Text: "restored " + binding.Mode + " to " + binding.Point.ID})
	a.publishState()
	return "restored " + binding.Mode + " to " + binding.Point.ID, nil
}

// Capture errors stop protected turns before model tools can change files.
func (a *Agent) beginAutomaticCheckpoint(ctx context.Context, turnID, text, inputID string) (func(), error) {
	a.mu.Lock()
	disabled := a.cfg.AutomaticCheckpoints != nil && !*a.cfg.AutomaticCheckpoints
	a.mu.Unlock()
	if disabled {
		return func() {}, nil
	}
	if a.childState != nil || a.persistenceHandle() == nil {
		return func() {}, nil
	}
	p, e := a.captureRecoveryPoint(ctx, turnID, text, inputID)
	if e != nil {
		return nil, e
	}
	return func() {
		if e := a.sealRecoveryPoint(p); e != nil {
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
	root := a.cfg.Workspace
	sb := a.sandbox.Snapshot()
	turn := fmt.Sprintf("turn-%d", a.turnSeq)
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
	if _, e = a.persistenceHandle().commitEvents(m.ID+"-before", m); e != nil {
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
		_, e = a.persistenceHandle().commitEvents(m.ID+"-after", m)
		workspace.Invalidate(root)
		return e
	}, nil
}

func (a *Agent) inverseMutationPlan(point session.RecoveryPointRecorded, current changes.Snapshot, selection []string) (changes.Plan, error) {
	store := a.codingState().store
	records, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		return changes.Plan{}, e
	}
	mutations := map[string]session.FileMutationRecorded{}
	order := []string{}
	found := false
	for _, r := range records {
		event := r.Event
		switch v := event.(type) {
		case session.RecoveryPointRecorded:
			event = &v
		case session.FileMutationRecorded:
			event = &v
		}
		switch v := event.(type) {
		case *session.RecoveryPointRecorded:
			if v.ID == point.ID {
				found = true
			}
		case *session.FileMutationRecorded:
			if found && v.Workspace == point.Workspace {
				if _, ok := mutations[v.ID]; !ok {
					order = append(order, v.ID)
				}
				mutations[v.ID] = *v
			}
		}
	}
	virtual := current
	virtual.Files = map[string]changes.File{}
	for p, f := range current.Files {
		virtual.Files[p] = f
	}
	conflicts := map[string]string{}
	conflictTargets := map[string]changes.File{}
	baseline, e := store.LoadSnapshot(point.Before)
	if e != nil {
		return changes.Plan{}, e
	}
	points, e := a.recoveryPoints()
	if e != nil {
		return changes.Plan{}, e
	}
	uncertain := func(path string) {
		conflicts[path] = "unattributed change (external command or concurrent edit); choose explicitly"
		target, ok := baseline.Files[path]
		if !ok {
			target = changes.File{Kind: "absent"}
		}
		conflictTargets[path] = target
	}
	for n, rp := range points {
		if rp.CreatedAt.Before(point.CreatedAt) {
			continue
		}
		before, err := store.LoadSnapshot(rp.Before)
		if err != nil {
			return changes.Plan{}, err
		}
		expected := before
		for _, id := range order {
			m := mutations[id]
			if m.CreatedAt.Before(rp.CreatedAt) || (n > 0 && !m.CreatedAt.Before(points[n-1].CreatedAt)) {
				continue
			}
			var b, after changes.File
			if err = store.Value(m.Before, &b); err != nil {
				return changes.Plan{}, err
			}
			existing, ok := expected.Files[m.Path]
			if !ok {
				existing = changes.File{Kind: "absent"}
			}
			if existing.Kind != b.Kind || existing.Digest != b.Digest || existing.Mode != b.Mode {
				uncertain(m.Path)
			}
			if m.After == "" {
				uncertain(m.Path)
				continue
			}
			if err = store.Value(m.After, &after); err != nil {
				return changes.Plan{}, err
			}
			if after.Kind == "absent" {
				delete(expected.Files, m.Path)
			} else {
				expected.Files[m.Path] = after
			}
		}
		after := current
		if rp.After != "" {
			after, err = store.LoadSnapshot(rp.After)
			if err != nil {
				return changes.Plan{}, err
			}
		}
		for _, edit := range changes.Difference(expected, after) {
			uncertain(edit.Path)
		}
	}

	for i := len(order) - 1; i >= 0; i-- {
		m := mutations[order[i]]
		if m.After == "" {
			conflicts[m.Path] = "mutation has no confirmed post-state"
			continue
		}
		var before, after changes.File
		if e = store.Value(m.Before, &before); e != nil {
			return changes.Plan{}, e
		}
		if e = store.Value(m.After, &after); e != nil {
			return changes.Plan{}, e
		}
		if before.Kind == after.Kind && before.Mode == after.Mode && before.Digest == after.Digest {
			continue
		}
		base := changes.Snapshot{Files: map[string]changes.File{m.Path: after}}
		target := changes.Snapshot{Files: map[string]changes.File{m.Path: before}}
		p, e := store.BuildPlan(base, target, virtual)
		if e != nil {
			return changes.Plan{}, e
		}
		for _, v := range p.Edits {
			if v.Conflict != "" {
				conflicts[v.Path] = v.Conflict
				conflictTargets[v.Path] = v.After
			} else if v.After.Kind == "absent" {
				delete(virtual.Files, v.Path)
			} else {
				virtual.Files[v.Path] = v.After
			}
		}
	}
	p, e := store.BuildPlan(current, virtual, current)
	if e != nil {
		return p, e
	}
	for path, reason := range conflicts {
		now := current.Files[path]
		if now.Kind == "" {
			now.Kind = "absent"
		}
		target, ok := conflictTargets[path]
		if !ok {
			target = now
		}
		filtered := p.Edits[:0]
		for _, edit := range p.Edits {
			if edit.Path != path {
				filtered = append(filtered, edit)
			}
		}
		p.Edits = filtered
		p.Edits = append(p.Edits, changes.Edit{Path: path, Before: now, After: target, Conflict: reason})
	}
	return store.SelectPlan(p, selection)
}
