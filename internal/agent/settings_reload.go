package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

// ReloadSettings re-reads the per-project settings (.ccdp/settings.json +
// settings.local.json) and applies the workspace-scoped keys at runtime
// (Claude Code's settings hot-reload): permission policy, hooks, sandbox mode.
func (a *Agent) ReloadSettings() error {
	workspace, err := a.reserveSettingsMutation()
	if err != nil {
		return err
	}
	defer a.releaseSettingsMutation()
	if err := a.reloadSettingsContext(a.lifecycle.rootCtx, workspace); err != nil {
		return err
	}
	a.emitStatus("settings reloaded from .ccdp/settings.json")
	return nil
}

func (a *Agent) reloadSettingsContext(ctx context.Context, workspace string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	candidate, err := a.prepareSettingsCandidate(workspace)
	if err != nil {
		return err
	}
	if err := a.commitSettingsCandidateContext(ctx, candidate); err != nil {
		return err
	}
	a.emit(Event{Type: EventModeChanged, Mode: candidate.mode})
	a.emit(Event{Type: EventSandboxChanged})
	return nil
}

// TrustProject records an explicit user authorization for the current
// workspace's executable project settings in the external trust store. The
// operation does not run hooks or start MCP; callers should reload after a
// successful authorization to prepare and publish the candidate.
func (a *Agent) TrustProject() error {
	a.mu.Lock()
	workspace := a.services.cfg.Workspace
	store := a.services.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	project, err := config.LoadProjectSettings(workspace)
	if err != nil {
		return err
	}
	if _, err := store.AuthorizeProject(workspace, project); err != nil {
		return err
	}
	return nil
}

// RevokeProjectTrust removes the explicit external authorization. Current
// bindings remain until an idle ReloadSettings publishes the now-safe
// candidate; this avoids mutating a live step in place.
func (a *Agent) RevokeProjectTrust() error {
	a.mu.Lock()
	workspace := a.services.cfg.Workspace
	store := a.services.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	return store.RevokeProject(workspace)
}

// trustProjectContext applies or revokes project trust and publishes the
// corresponding settings candidate as one idle operation. If candidate
// preparation fails, restore the previous trust record so an authorization
// command cannot leave an unexplained half-applied state.
func (a *Agent) trustProjectContext(ctx context.Context, revoke bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	a.mu.Lock()
	workspace := a.services.cfg.Workspace
	store := a.services.trustStore
	a.mu.Unlock()
	if store == nil {
		store = config.DefaultTrustStore()
	}
	project, err := config.LoadProjectSettings(workspace)
	if err != nil {
		return err
	}
	trusted, _, err := store.Check(workspace, project)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if revoke {
		if err := store.RevokeProject(workspace); err != nil {
			return err
		}
		if err := a.reloadSettingsContext(ctx, workspace); err != nil {
			if trusted {
				_, _ = store.AuthorizeProject(workspace, project)
			}
			return err
		}
		return nil
	}
	if _, err := store.AuthorizeProject(workspace, project); err != nil {
		return err
	}
	if err := a.reloadSettingsContext(ctx, workspace); err != nil {
		if trusted {
			_, _ = store.AuthorizeProject(workspace, project)
		} else {
			_ = store.RevokeProject(workspace)
		}
		return err
	}
	return nil
}

// reserveSettingsMutation closes the check-to-publish race for /reload and
// /cd. The reservation is represented by the existing busy owner state, so
// input admission queues rather than starting a turn and model/policy
// commands cannot observe a half-published candidate.
type settingsReservationResult struct {
	workspace string
	err       error
}
type settingsReservationRequested struct {
	result chan settingsReservationResult
}

func (settingsReservationRequested) ownerControlMessage() {}

type settingsReservationReleased struct{ done chan struct{} }

func (settingsReservationReleased) ownerControlMessage() {}

func (a *Agent) reserveSettingsMutation() (string, error) {
	a.ensureRun()
	request := settingsReservationRequested{result: make(chan settingsReservationResult, 1)}
	select {
	case a.owner.control <- request:
	case <-a.lifecycle.rootCtx.Done():
		return "", a.lifecycle.rootCtx.Err()
	}
	select {
	case result := <-request.result:
		return result.workspace, result.err
	case <-a.owner.done:
		return "", fmt.Errorf("session owner is closed")
	}
}

func (a *Agent) reserveSettingsMutationOwned() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turn.busy || a.lifecycle.settling || a.capabilities.settingsPreparing || a.lifecycle.closing || a.lifecycle.closed {
		return "", fmt.Errorf("settings mutation requires an idle session")
	}
	workspace := a.services.cfg.Workspace
	a.turn.busy = true
	a.capabilities.settingsPreparing = true
	a.lifecycle.operationWG.Add(1)
	a.turn.workStartedAt = time.Now()
	a.turn.workLabel = ""
	a.lifecycle.phase = protocol.PhasePreparing
	return workspace, nil
}

func (a *Agent) releaseSettingsMutation() {
	done := make(chan struct{})
	// Admission reserves operationWG, so Close keeps the owner alive for release.
	a.owner.control <- settingsReservationReleased{done: done}
	<-done
	a.lifecycle.operationWG.Done()
}

func (a *Agent) releaseSettingsMutationOwned() {
	a.mu.Lock()
	if a.turn.busy && a.turn.turnCancel == nil && !a.lifecycle.closing && !a.lifecycle.closed {
		a.turn.busy = false
		a.lifecycle.phase = protocol.PhaseIdle
	}
	a.mu.Unlock()
	a.finishSettingsPreparation()
	a.publishState()
}
