package agent

import (
	"errors"
	"fmt"

	"ccdp/internal/session"
)

// persistSettingsFact is the narrow Agent-facing boundary for command and
// provider code that changes public session settings. It serializes with
// history/tool facts and never accepts credentials (session.Settings has no
// API-key or Authorization fields).
func (a *Agent) persistSettingsFact(settings session.Settings, revision uint64) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := p.PersistSettingsSnapshot(settings, revision); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist settings: %w", err))
	}
	return nil
}

func (a *Agent) sessionSettingsLocked() session.Settings {
	settings := session.Settings{}
	if a == nil {
		return settings
	}
	settings.Model = a.activeModelLocked()
	settings.Provider = a.modelsState.activeBinding.provider
	if settings.Provider == "" && a.services.client != nil {
		settings.Provider = a.services.client.Name()
	}
	settings.Endpoint = a.modelsState.activeBinding.endpoint
	if settings.Endpoint == "" {
		settings.Endpoint = a.services.cfg.BaseURL
	}
	if endpoint, err := sanitizeRequestEndpoint(settings.Endpoint); err == nil {
		settings.Endpoint = endpoint
	} else {
		settings.Endpoint = ""
	}
	settings.Workspace = a.services.cfg.Workspace
	settings.PermissionPolicy = a.services.cfg.PermissionMode
	settings.AlwaysAllow = append([]string(nil), a.services.cfg.AlwaysAllow...)
	settings.AlwaysDeny = append([]string(nil), a.services.cfg.AlwaysDeny...)
	settings.NetworkAccess = a.services.cfg.NetworkAccess
	settings.AdditionalDirectories = append([]string(nil), a.services.cfg.AdditionalDirectories...)
	settings.AdditionalReadOnlyDirectories = append([]string(nil), a.services.cfg.AdditionalReadOnlyDirectories...)
	settings.DisallowedDirectories = append([]string(nil), a.services.cfg.DisallowedDirectories...)
	settings.ContextWindow = a.services.cfg.ContextWindow
	settings.EffectiveContextWindow = a.contextWindowLocked()
	settings.CompactThreshold = a.services.cfg.CompactThreshold
	settings.MaxResultSizeChars = a.services.cfg.MaxResultSizeChars
	settings.MaxTurns = a.services.cfg.MaxTurns

	maxOutputTokens := a.services.cfg.MaxOutputTokensFor(a.activeModelLocked())
	settings.MaxOutputTokens = &maxOutputTokens
	settings.ReasoningEffort = a.services.cfg.ReasoningEffort
	settings.Verbosity = a.services.cfg.Verbosity
	settings.GenerationOptionsSet = true
	settings.MaxToolOutputCharsPerTurn = a.services.cfg.MaxToolOutputCharsPerTurn
	if a.plan.planMode {
		settings.ExecutionMode = "plan"
	} else {
		settings.ExecutionMode = "execute"
	}
	return settings
}

// persistSettingsWorkflowFact commits settings and its workflow companion as
// one typed transaction. Callers use it before publishing/mutating either
// projection, so an injected store failure cannot expose a half-applied
// command.
func (a *Agent) persistSettingsWorkflowFact(settings session.Settings, revision uint64, workflow *session.WorkflowState) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := p.PersistSettingsWorkflow(settings, revision, workflow); err != nil {
		return a.toolJournalFailure(err)
	}
	return nil
}

func (a *Agent) persistWorkflowFact(workflow session.WorkflowState) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := p.PersistWorkflow(workflow); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist workflow: %w", err))
	}
	return nil
}

func (a *Agent) persistCommandCompletedFact(event session.CommandCompleted) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := p.PersistCommandCompleted(event); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command completion: %w", err))
	}
	return nil
}

func (p *sessionPersistence) persistUsage(u Usage) error {
	return p.Writer.PersistUsage(session.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CachedTokens: int64(u.CachedTokens), TurnCount: int64(u.TurnCount), TotalTokens: int64(u.InputTokens) + int64(u.OutputTokens), Cache: u.Cache})
}
