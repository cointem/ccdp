package agent

import (
	"errors"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (a *Agent) applyExecutionMode(cmd protocol.Command, owned ...bool) protocol.Receipt {
	on := cmd.ExecutionMode.Mode == protocol.ExecutionModePlan
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		// PermissionPolicy already holds the concrete base mode; a plan toggle only
		// moves the execution-mode dimension and preserves that base.
		if on {
			target.ExecutionMode = "plan"
		} else {
			target.ExecutionMode = "execute"
		}
		return nil
	}, owned...)
}

type executionModeRequested struct {
	on     bool
	result chan error
}

func (executionModeRequested) ownerControlMessage() {}

func (a *Agent) setExecutionMode(on bool) error {
	a.ensureRun()
	request := executionModeRequested{on: on, result: make(chan error, 1)}
	select {
	case a.owner.control <- request:
	case <-a.lifecycle.rootCtx.Done():
		return a.lifecycle.rootCtx.Err()
	}
	// Once admitted, closing the execution context cannot abandon settlement.
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return errors.New("session closed before mode change settled")
	}
}

func (a *Agent) installExecutionMode(on bool) error {
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	a.mu.Lock()
	if a.lifecycle.closing || a.lifecycle.closed {
		a.mu.Unlock()
		return errors.New("session is closing")
	}
	base := a.plan.planBaseMode
	if on {
		if !a.plan.planMode {
			a.plan.planBaseMode = a.services.perms.CurrentMode()
			if a.plan.planBaseMode == permissions.ModePlan {
				a.plan.planBaseMode = permissions.ModeDefault
			}
		}
		base = a.plan.planBaseMode
		// The candidate is persisted before any public mode/workflow mutation.
	} else {
		if base == "" {
			base = permissions.ModeDefault
		}
	}
	nextRevision := a.sequences.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	settings := a.sessionSettingsLocked()
	if on {
		settings.ExecutionMode = "plan"
	} else {
		settings.ExecutionMode = "execute"
	}
	settings.PermissionPolicy = string(base)
	workflow := session.WorkflowState{Phase: string(protocol.WorkflowOff)}
	if on {
		workflow.Phase = string(protocol.WorkflowDrafting)
	}
	a.mu.Unlock()
	if err := a.persistSettingsWorkflowFact(settings, nextRevision, &workflow); err != nil {
		return err
	}
	a.mu.Lock()
	if on {
		if !a.plan.planMode {
			a.plan.planBaseMode = base
		}
		a.plan.planMode = true
		a.lifecycle.workflow = protocol.WorkflowDrafting
	} else {
		a.plan.planMode = false
		a.lifecycle.workflow = protocol.WorkflowOff
		a.plan.planBaseMode = base
	}
	a.sequences.settingsRev = nextRevision
	a.services.cfg.PermissionMode = string(base)
	a.services.baseCfg.PermissionMode = string(base)
	a.mu.Unlock()
	a.services.perms.SetMode(base)
	a.syncHookContext()
	a.emit(Event{Type: EventPlanModeChanged, PlanMode: on})
	a.emitStatus("plan mode %s", map[bool]string{true: "on", false: "off"}[on])
	a.publishState()
	return nil
}

func (a *Agent) applyPermissionPolicy(cmd protocol.Command, owned ...bool) protocol.Receipt {
	p := cmd.PermissionPolicy.Policy
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		currentMode := permissions.Mode(target.PermissionPolicy)
		plan := target.ExecutionMode == "plan"
		mode := currentMode
		if p.Mode != "" {
			parsed, err := permissions.ParseMode(p.Mode)
			if err != nil {
				return err
			}
			mode = parsed
		}
		if mode == permissions.ModePlan {
			// /mode plan is a compatibility entry into the independent plan
			// workflow. Preserve the current base policy, which may have changed
			// since planBaseMode was captured at startup or a previous plan entry.
			mode = currentMode
			if mode == permissions.ModePlan || mode == "" {
				mode = permissions.ModeDefault
			}
			plan = true
		}
		allow := target.AlwaysAllow
		if p.AlwaysAllow != nil {
			allow = p.AlwaysAllow
		}
		deny := target.AlwaysDeny
		if p.AlwaysDeny != nil {
			deny = p.AlwaysDeny
		}
		target.PermissionPolicy = string(mode)
		target.AlwaysAllow = cloneStringSlice(allow)
		target.AlwaysDeny = cloneStringSlice(deny)
		if plan {
			target.ExecutionMode = "plan"
		} else {
			target.ExecutionMode = "execute"
		}
		return nil
	}, owned...)
}

func (a *Agent) applySandboxPolicy(cmd protocol.Command, owned ...bool) protocol.Receipt {
	p := cmd.SandboxPolicy.Policy
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		target.NetworkAccess = p.NetworkAccess
		target.AdditionalDirectories = cloneStringSlice(p.AdditionalDirectories)
		target.AdditionalReadOnlyDirectories = cloneStringSlice(p.AdditionalReadOnlyDirectories)
		target.DisallowedDirectories = cloneStringSlice(p.DisallowedDirectories)
		return nil
	}, owned...)
}
