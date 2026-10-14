package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

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
		root := a.services.cfg.Workspace
		a.mu.Unlock()
		resolved, _ := filepath.EvalSymlinks(root)
		if point.Workspace != resolved {
			return "", errors.New("recovery point belongs to another workspace")
		}
		current, e := a.captureRestoreFiles(ctx, point, w.Mode)
		if e != nil {
			return "", e
		}
		var plan changes.Plan
		if w.Mode == "conversation" {
			plan, e = state.store.Prepare(current, current, current, nil)
		} else if w.Scope == "snapshot" {
			if point.Before == "" {
				return "", errors.New("this recovery point contains file-tool edits only; no full workspace snapshot was captured")
			}
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
	if a.services.resources != nil && a.services.resources.Processes.Count() > 0 {
		return "", errors.New("stop managed background processes before restoring")
	}
	if restoration.Status == "" {
		before, err := a.captureRestoreFiles(ctx, binding.Point, binding.Mode)
		if err != nil {
			return "", err
		}
		if binding.Mode == "conversation" || binding.Point.Before == "" {
			before.ID = ""
		}
		backup, err := a.captureRecoveryContext(before, "restore-"+w.ID, "Before restoring "+binding.Point.ID)
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
		if _, err = a.commitAuxiliary("restore-start-"+w.ID, restoration); err != nil {
			return "", err
		}
	}

	if binding.Mode == "conversation" || binding.Mode == "both" {
		var target []messages.Message
		if err := state.store.Value(restoration.History, &target); err != nil {
			return "", err
		}
		if err := a.persistenceHandle().ValidateHistoryReplacement(target, "restore-"+w.ID); err != nil {
			return "", fmt.Errorf("restore conversation: %w", err)
		}
		if a.services.supervisor != nil {
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
		if err := a.cancelRunInputsFromWorker("restore-" + w.ID); err != nil {
			return "", err
		}
	}
	if binding.Mode != "conversation" {
		for _, edit := range binding.Plan.Edits {
			if edit.Conflict != "" {
				return "", changes.ErrConflict
			}
			if e := a.authorizeCommand(ctx, cmd.ID, "Write", map[string]any{"file_path": filepath.Join(binding.Plan.Root, edit.Path), "restore_plan": w.ID}, permissions.KindWrite); e != nil {
				return "", e
			}
		}
		a.mu.Lock()
		sb := a.services.sandbox.Snapshot()
		a.mu.Unlock()
		j, e := state.store.ApplyWithRecorder(ctx, binding.Plan, sb, a.recordCodingWrite)
		if e != nil {
			b, _ := json.Marshal(j)
			return string(b), e
		}
	}
	var history []messages.Message
	if binding.Mode == "code" {
		a.mu.Lock()
		history = messages.Clone(a.historyLocked())
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
	e = a.replaceHistoryFromWorker(historyReplacementPrepared{history: history, turnID: "restore-" + w.ID})
	if e != nil {
		return "files restored; conversation recording requires recover", e
	}
	restoration.Status = "completed"
	if _, e = a.commitAuxiliary("restore-complete-"+w.ID, restoration); e != nil {
		return "files restored; completion requires recover", e
	}

	if a.services.resources != nil {
		a.services.resources.Files.Clear()
	}
	workspace.Invalidate(binding.Plan.Root)
	a.emit(Event{Type: EventHistoryChanged, Text: "restored " + binding.Mode + " to " + binding.Point.ID})
	a.publishState()
	return "restored " + binding.Mode + " to " + binding.Point.ID, nil
}
