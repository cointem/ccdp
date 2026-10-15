package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/config"
	"ccdp/internal/fsops"
	"ccdp/internal/mcp"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func (parent *Agent) prepareChild(purpose, system string) (config.Config, Options, *mcp.StepLease, *tools.Lease, []plugin.PreToolHook, error) {
	parent.settingsCommitMu.Lock()
	defer parent.settingsCommitMu.Unlock()
	cfg, opts, err := childOptionsFromParent(parent, purpose)
	if err != nil {
		return cfg, opts, nil, nil, nil, err
	}
	parent.mu.Lock()
	var lease *mcp.StepLease
	var catalog *tools.Lease
	var hooks []plugin.PreToolHook
	expired := false
	if step := parent.children.childStep; step != nil && parent.turn.busy && step.turn == parent.sequences.turnSeq {
		lease = step.mcpLease.Retain()
		expired = step.mcpLease != nil && lease == nil
		catalog = step.toolLease
		hooks = append(hooks, step.preHooks...)
	} else if parent.services.mcp != nil {
		lease = parent.services.mcp.AcquireStep(parent.services.registry)
		catalog = lease.Tools
		hooks = parent.services.gohooks.SnapshotPreTool()
	} else {
		catalog = parent.services.registry.Acquire()
		hooks = parent.services.gohooks.SnapshotPreTool()
	}
	parent.mu.Unlock()
	if expired {
		return cfg, opts, nil, nil, nil, errors.New("parent step lease ended while preparing child; retry at the next safe boundary")
	}
	if lease != nil {
		catalog = lease.Tools
	}
	if system == "" {
		system = SubagentDefaultSystem
	}
	if purpose == childPurposeGuardian {
		system = GuardianSystem
	}
	cfg.SystemPrompt = system
	opts.goalOwner, opts.goalOwnerID = parent.goalAccount()
	return cfg, opts, lease, catalog, hooks, nil
}

func (s *SessionSupervisor) prepare(parent childParent, purpose, system string) (config.Config, Options, *mcp.StepLease, *tools.Lease, []plugin.PreToolHook, error) {
	cfg, opts, lease, catalog, hooks, err := parent.prepareChild(purpose, system)
	if err != nil {
		return cfg, opts, lease, catalog, hooks, err
	}
	opts.Supervisor = s
	opts.RootContext = s.root.rootContext()
	opts.NonInteractive = !s.directory.Interactive() || purpose == childPurposeGuardian
	return cfg, opts, lease, catalog, hooks, nil
}

func (s *SessionSupervisor) launch(parent *Agent, ctx context.Context, task childTask, purpose string, callID string) (*managedRun, error) {
	return s.launchWithReview(parent, ctx, task, purpose, callID, nil)
}

func (s *SessionSupervisor) launchWithReview(parent *Agent, ctx context.Context, task childTask, purpose string, callID string, review *protocol.Command) (*managedRun, error) {
	if ctx == nil {
		ctx = parent.lifecycle.rootCtx
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var taskErr error
	task, taskErr = resolveChildTask(task)
	if taskErr != nil {
		return nil, taskErr
	}
	if purpose == childPurposeGuardian {
		task.Role = "explorer"
	}
	if strings.TrimSpace(task.Description) == "" {
		return nil, errors.New("child: description is required")
	}
	cfg, opts, lease, catalog, hooks, err := s.prepare(parent, purpose, task.SystemPrompt)
	if err != nil {
		return nil, err
	}
	policy := task.WaitPolicy
	if policy == "" {
		policy = "join"
	}
	if policy != "join" && policy != "notify" && policy != "observe" {
		lease.Close()
		return nil, errors.New("child: wait_policy must be join or notify")
	}
	sid := protocol.SessionID(newSessionID())
	cfg.SessionID = string(sid)
	var childWorkspace, baselineID string
	var workspaceLease *atomicfile.Lock
	queued := false
	if isTaskChild(purpose) {
		childWorkspace, baselineID, err = parent.prepareChildWorkspace(ctx, task, &cfg, &opts)
		if err != nil {
			lease.Close()
			return nil, err
		}
		if baselineID != "" {
			workspaceLease, err = fsops.LeaseWorkspace(childWorkspace)
			if err != nil {
				lease.Close()
				return nil, err
			}
			defer func() {
				if !queued {
					_ = parent.removeChildWorkspace(context.Background(), childWorkspace)
					workspaceLease.Close()
				}
			}()
		}
	}
	parent.mu.Lock()
	turnID := fmt.Sprintf("%d", parent.sequences.turnSeq)
	parent.mu.Unlock()
	row := protocol.ChildSession{SessionID: sid, RootSessionID: protocol.SessionID(s.root.SessionID()), ParentSessionID: protocol.SessionID(parent.conversation.sessionID),
		Role: task.Role, WorkspaceMode: task.WorkspaceMode, Name: task.Name, Workspace: childWorkspace, BaselineID: baselineID,
		DelegationID: protocol.DelegationID(nextRuntimeID("delegation")), ParentTurnID: protocol.TurnID(turnID), ParentCallID: protocol.CallID(callID),
		Title: task.Description, Purpose: purpose, CreatedAt: time.Now().UTC(),
		Run: protocol.RunView{ID: protocol.RunID(nextRuntimeID("run")), Status: "queued", WaitPolicy: policy}}
	if policy == "notify" {
		ctx = s.root.rootContext()
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &managedRun{workspaceLease: workspaceLease, reviewCommand: review, fact: session.ChildRunRecorded{Version: 1, Child: row, SystemPrompt: cfg.SystemPrompt},
		parent: parent, cfg: cfg, opts: opts, lease: lease, toolLease: catalog, preHooks: hooks, cancel: cancel, done: make(chan struct{})}
	bindChildGoal(&r.fact, opts)
	if err := s.reserveRun(r, nil); err != nil {
		cancel()
		lease.Close()
		return nil, err
	}
	r.mu.Lock()
	err = s.persist(r, "queued")
	r.mu.Unlock()
	s.finishRunAdmission(r, err)
	if err != nil {
		cancel()
		lease.Close()
		return nil, err
	}
	queued = true
	if task.ContextMode == "fork" {
		parent.mu.Lock()
		r.forkHistory = cloneForkHistory(parent.conversation.durable.History)
		parent.mu.Unlock()
	}
	go s.execute(r, runCtx, task.Description, nil)
	return r, nil
}
