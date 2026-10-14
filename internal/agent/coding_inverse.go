package agent

import (
	"context"

	"ccdp/internal/changes"
	"ccdp/internal/session"
)

func (a *Agent) mutationsAfter(point session.RecoveryPointRecorded) (map[string]session.FileMutationRecorded, []string, error) {
	records, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		return nil, nil, e
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
	return mutations, order, nil
}

func (a *Agent) inverseMutationPlan(point session.RecoveryPointRecorded, current changes.Snapshot, selection []string) (changes.Plan, error) {
	store := a.codingState().store
	mutations, order, e := a.mutationsAfter(point)
	if e != nil {
		return changes.Plan{}, e
	}
	virtual := current
	virtual.Files = map[string]changes.File{}
	for p, f := range current.Files {
		virtual.Files[p] = f
	}
	conflicts := map[string]string{}
	conflictTargets := map[string]changes.File{}
	if point.Before != "" {
		baseline, e := store.LoadSnapshot(point.Before)
		if e != nil {
			return changes.Plan{}, e
		}
		points, e := a.recoveryPoints()
		if e != nil {
			return changes.Plan{}, e
		}
		// Only full snapshots bound snapshot attribution intervals. A lazy
		// file-tool checkpoint must not hide mutations in a full interval.
		fullPoints := points[:0]
		for _, rp := range points {
			if rp.Before != "" {
				fullPoints = append(fullPoints, rp)
			}
		}
		points = fullPoints
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
			afterID := rp.After
			// An unsealed manual checkpoint ends at the next newer full
			// checkpoint, not at today's workspace state.
			if afterID == "" && n > 0 {
				afterID = points[n-1].Before
			}
			if afterID != "" {
				after, err = store.LoadSnapshot(afterID)
				if err != nil {
					return changes.Plan{}, err
				}
			}
			for _, edit := range changes.Difference(expected, after) {
				uncertain(edit.Path)
			}
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

// File-only restore reads only paths recorded by file tools. Unrelated files
// (including large or unreadable files) never become restore prerequisites.
func (a *Agent) captureRestoreFiles(ctx context.Context, point session.RecoveryPointRecorded, mode string) (changes.Snapshot, error) {
	if mode != "conversation" && point.Before != "" {
		return a.captureCoding(ctx)
	}
	paths := []string{}
	if mode != "conversation" {
		mutations, _, err := a.mutationsAfter(point)
		if err != nil {
			return changes.Snapshot{}, err
		}
		seen := map[string]bool{}
		for _, mutation := range mutations {
			if !seen[mutation.Path] {
				seen[mutation.Path] = true
				paths = append(paths, mutation.Path)
			}
		}
	}
	a.mu.Lock()
	policy := a.services.sandbox.Snapshot()
	a.mu.Unlock()
	return a.codingState().store.CapturePaths(ctx, point.Workspace, paths, policy)
}
