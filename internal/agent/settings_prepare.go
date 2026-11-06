package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/execution"
	"ccdp/internal/mcp"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

// settingsPreparation contains only the captured generation and resources to
// retire. Preparation never reads or changes the live session projection.
type settingsPreparation struct {
	target         session.Settings
	config         config.Config
	models         *plugin.ModelRegistry
	active         modelBinding
	sourceRevision uint64
	quiescence     *sandboxQuiescence
}

type settingsPrepared struct {
	target         session.Settings
	binding        *modelBinding
	sourceRevision uint64
	quiesced       bool
	err            error
}

func prepareSettings(input settingsPreparation) settingsPrepared {
	result := settingsPrepared{target: input.target, sourceRevision: input.sourceRevision}
	if input.quiescence != nil {
		if err := input.quiescence.drain(); err != nil {
			result.err = fmt.Errorf("sandbox policy change is waiting for old executions to stop; dispatch remains blocked: %w", err)
			return result
		}
		result.quiesced = true
	}
	if input.target.Model != input.active.model || input.target.Provider != input.active.provider || input.target.Endpoint != input.active.endpoint {
		binding, err := providerBinding(input.config, input.models, input.target.Model, input.active.version+1)
		if err != nil {
			result.err = err
			return result
		}
		result.binding = &binding
		result.target.Model, result.target.Provider = binding.model, binding.provider
		result.target.Endpoint, _ = sanitizeRequestEndpoint(binding.endpoint)
	}
	return result
}

// awaitSettings keeps lifecycle completions and user controls responsive while
// external work drains. Settings and ordinary commands retain their FIFO order
// and original ExpectedRevision. A close barrier cannot pass a pending install.
func (a *Agent) awaitSettings(done <-chan settingsPrepared) settingsPrepared {
	for {
		a.drainRejectedCommands()
		a.drainChildRecords()
		a.drainOperationCompletions()
		a.drainViews()
		commands := a.owner.commands
		if len(a.owner.deferred) >= cap(a.owner.commands) {
			commands = nil
		}
		select {
		case result := <-done:
			return result
		case message := <-a.owner.control:
			a.handleOwnerControl(message, true)
		case req := <-commands:
			switch req.command.Type {
			case protocol.CommandInterrupt, protocol.CommandStop, protocol.CommandApproveTool, protocol.CommandAnswerQuestion:
				a.dispatchOwned(req)
			default:
				a.owner.deferred = append(a.owner.deferred, req)
			}
		}
	}
}

// sandboxQuiescence owns a frozen set of cancellation and join handles. These
// are resource operations, never callbacks that can mutate session state.
type sandboxQuiescence struct {
	revision         uint64
	turnCancel       context.CancelFunc
	resources        *tools.Resources
	mcp              *mcp.Manager
	supervisor       *SessionSupervisor
	policy           *sandbox.Sandbox
	managedStopped   bool
	turn, operations interface{ Wait() }
}

func (a *Agent) beginSandboxQuiescence(ownerCtx context.Context, closeMCP bool, allowPending ...bool) (*sandboxQuiescence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.capabilities.capabilityRevoking && (len(allowPending) == 0 || !allowPending[0]) {
		return nil, errors.New("sandbox capability revocation is already pending")
	}
	a.capabilities.capabilityRevoking = true
	a.capabilities.capabilityRevision++
	a.capabilities.revocationRevision++
	scope := &sandboxQuiescence{revision: a.capabilities.capabilityRevision, turnCancel: a.turn.turnCancel, resources: a.services.resources, supervisor: a.services.supervisor, policy: a.services.sandbox, turn: &a.lifecycle.turnWG, operations: &a.lifecycle.sandboxOperationWG}
	if scope.policy != nil {
		scope.policy = scope.policy.Snapshot()
		for _, grant := range a.capabilities.capabilityGrants {
			if err := applyCapability(scope.policy, grant); err != nil {
				return nil, err
			}
		}
	}
	if ownerCtx != nil && a.turn.turnCtx == ownerCtx {
		scope.turnCancel = nil
	}
	if closeMCP {
		scope.mcp = a.services.mcp
	}
	return scope, nil
}

// Acknowledgement from the executor precedes reopening runtime admission.
// Failed cleanup/IPC keeps project execution closed without disabling chat.
func (a *Agent) resumeSandboxAdmission() error {
	policy := a.currentSandbox()
	if err := execution.ResumeAdmission(policy); err != nil {
		a.mu.Lock()
		a.capabilities.capabilityRevoking = true
		a.mu.Unlock()
		a.emitStatus("execution remains paused: %v", err)
		return err
	}
	a.mu.Lock()
	a.capabilities.capabilityRevoking = false
	resources := a.services.resources
	a.mu.Unlock()
	if resources != nil && resources.Processes != nil {
		resources.Processes.ResumeAfterRevocation()
	}
	return nil
}

func (scope *sandboxQuiescence) drain() error {
	var stopErr error
	gateCtx, gateCancel := context.WithTimeout(context.Background(), 8*time.Second)
	stopErr = execution.Quarantine(gateCtx, scope.policy)
	gateCancel()
	if scope.turnCancel != nil {
		scope.turnCancel()
	}
	if scope.resources != nil && scope.resources.Processes != nil {
		if err := scope.resources.Processes.StopAll(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("stop background processes: %w", err))
		}
	}
	if scope.mcp != nil {
		if err := scope.mcp.Close(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("close MCP connections: %w", err))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if scope.supervisor != nil {
		if err := scope.supervisor.revokeCapabilities(ctx); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("stop descendant sessions: %w", err))
		}
	}
	if err := waitGroupContext(ctx, scope.turn); err != nil {
		stopErr = errors.Join(stopErr, fmt.Errorf("wait for active turn: %w", err))
	}
	if err := waitGroupContext(ctx, scope.operations); err != nil {
		stopErr = errors.Join(stopErr, fmt.Errorf("wait for active runtime operations: %w", err))
	}
	if scope.resources != nil {
		scope.resources.RetireFileWorkers()
	}
	if scope.policy != nil && scope.policy.ExternalExecutionPossible() {
		reason := "a local process may have descendants retaining the previous Seatbelt policy"
		stopErr = errors.Join(stopErr, scope.policy.RecordQuarantine(reason))
		scope.managedStopped = stopErr == nil
		stopErr = errors.Join(stopErr, errors.New(reason))
	} else {
		scope.managedStopped = stopErr == nil
	}
	return stopErr
}

func (a *Agent) finishSettingsPreparation() {
	a.mu.Lock()
	a.capabilities.settingsPreparing = false
	resume := !a.turn.busy && !a.lifecycle.settling && !a.lifecycle.closing && !a.lifecycle.closed && !a.turn.interruptFlag && a.services.persistenceErr == nil && (a.hasTurnStartingInputLocked() || a.goalRunnableLocked())
	sequence := a.sequences.turnSeq
	if resume {
		a.turn.busy = true
	}
	a.mu.Unlock()
	if resume {
		a.launchQueuedTurn(sequence)
	}
}

func (a *Agent) settingsPreparationPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capabilities.settingsPreparing
}
func settingsMutationCommand(kind protocol.CommandType) bool {
	switch kind {
	case protocol.CommandSetModel, protocol.CommandSetGeneration, protocol.CommandSetPermissionPolicy, protocol.CommandSetSandboxPolicy, protocol.CommandRevokeCapabilities, protocol.CommandReloadSettings, protocol.CommandSetWorkspace, protocol.CommandTrustProject, protocol.CommandProjectMemory:
		return true
	default:
		return false
	}
}
