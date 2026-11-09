package agent

import (
	"context"
	"os"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

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
	a.emit(protocol.EventView{Kind: protocol.EventStateChanged, PermissionMode: string(candidate.mode)})
	a.emit(protocol.EventView{Kind: protocol.EventStateChanged})
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
