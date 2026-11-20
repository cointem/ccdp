package agent

import (
	"context"
	"os"
	"slices"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

// Refresh the editor's model and permission settings at a request boundary.
// Like SetModel, this never restarts tools or changes a request already in flight.
func (a *Agent) reloadUserPreferences(cmd protocol.Command) protocol.Receipt {
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	a.mu.Lock()
	base, cfg := a.services.baseCfg.Clone(), a.services.cfg.Clone()
	active := a.modelsState.activeBinding
	models := a.services.models.Freeze()
	child := a.children.childState != nil
	nextRevision := a.sequences.settingsRev + 1
	a.mu.Unlock()
	if child || base.SourcePath() == "" {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "该会话没有可重新加载的用户配置")
	}
	saved, err := config.LoadFrom(base.SourcePath())
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	updateMode := cmd.Reload.PermissionModeChanged
	base.ApplyUserPreferences(saved, updateMode)
	// Match startup precedence without reloading unrelated hooks, tools or MCP.
	projectPrefs := base.Clone()
	projectPrefs.Workspace = cfg.Workspace
	if updateMode {
		project, err := config.LoadProjectSettingsTrusted(cfg.Workspace, cfg.TrustStore)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
		config.ApplyProjectSettings(&projectPrefs, &project)
		if err := projectPrefs.ApplyProjectPermission(); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
	}
	cfg.ApplyUserPreferences(projectPrefs, updateMode)
	model := active.model
	if cfg.ValidateModelSelection(model) != nil || active.providerID != "" && !cfg.ResolveProvider(model).Served {
		model = saved.Model
	}
	cfg.Model = model
	levels := cfg.ReasoningEffortsFor(model)
	if len(levels) != 0 && !cfg.SupportsReasoningEffort(model, cfg.ReasoningEffortFor(model)) {
		cfg.ReasoningEffort = config.DefaultReasoningEffort
		if !slices.Contains(levels, cfg.ReasoningEffort) {
			cfg.ReasoningEffort = levels[0]
		}
	}
	binding, err := providerBinding(cfg, models, model, active.version+1)
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	var fallback modelBinding
	if cfg.FallbackModel != "" {
		fallback, err = providerBinding(cfg, models, cfg.FallbackModel, binding.version)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
	}
	mode := permissions.Mode(cfg.PermissionMode)
	a.mu.Lock()
	settings := a.settingsForCandidateLocked(&settingsCandidate{cfg: cfg, binding: binding, mode: mode})
	a.mu.Unlock()
	if err := a.persistSettingsFact(settings, nextRevision); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.mu.Lock()
	a.services.baseCfg.ApplyUserPreferences(base, updateMode)
	a.services.cfg.ApplyUserPreferences(projectPrefs, updateMode)
	a.services.cfg.ReasoningEffort, a.services.baseCfg.ReasoningEffort = cfg.ReasoningEffort, cfg.ReasoningEffort
	a.applyBindingLocked(binding)
	a.configuredModel, a.modelsState.fallbackBinding = saved.Model, fallback
	a.sequences.settingsRev = nextRevision
	a.mu.Unlock()
	a.registerBindingRoutes(binding, cfg)
	if updateMode {
		a.syncSettingsPolicy(mode, nil)
	}
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
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
