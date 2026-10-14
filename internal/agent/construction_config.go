package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// applySessionSettingsToConfig restores credential-free settings under the
// current process configuration's authorization ceiling. Prior session
// settings may tighten today's baseline, but cannot restore network or
// directory access that the current configuration no longer authorizes.
func applySessionSettingsToConfig(cfg *config.Config, settings session.Settings) {
	if cfg == nil {
		return
	}
	launchOverride := func(field string) bool {
		source := cfg.SourceOf(field)
		return source == config.SourceCLI || source == config.SourceEnvironment
	}
	if settings.Workspace != "" {
		cfg.Workspace = settings.Workspace
	}
	if settings.Model != "" && !launchOverride("model") {
		cfg.Model = settings.Model
	}
	// settings.Endpoint is a record of which endpoint served the previous
	// request — possibly a provider-scoped or plugin endpoint — not a runtime
	// setting. Applying it to cfg.BaseURL would authenticate the top-level
	// credential against another provider's server, so the resumed binding is
	// always re-derived from the model plus the current config.
	if settings.PermissionPolicy != "" && !launchOverride("permission_mode") {
		current, _ := permissions.ParseMode(cfg.PermissionMode)
		saved, _ := permissions.ParseMode(settings.PermissionPolicy)
		if current == permissions.ModeBypass || (current != permissions.ModePlan && saved == permissions.ModeDefault) || saved == permissions.ModePlan || (current == permissions.ModeAcceptEdits && saved == permissions.ModeAcceptEdits) {
			cfg.PermissionMode = settings.PermissionPolicy
		}
	}
	if settings.ExecutionMode == string(protocol.ExecutionModePlan) && !launchOverride("permission_mode") {
		cfg.PermissionMode = string(permissions.ModePlan)
	}
	cfg.NetworkAccess = cfg.NetworkAccess && settings.NetworkAccess
	if settings.AlwaysAllow != nil && !launchOverride("always_allow") {
		var allowed []string
		for _, rule := range cfg.AlwaysAllow {
			if slices.Contains(settings.AlwaysAllow, rule) {
				allowed = append(allowed, rule)
			}
		}
		cfg.AlwaysAllow = allowed
	}
	if settings.AlwaysDeny != nil {
		for _, rule := range settings.AlwaysDeny {
			if !slices.Contains(cfg.AlwaysDeny, rule) {
				cfg.AlwaysDeny = append(cfg.AlwaysDeny, rule)
			}
		}
	}
	if settings.AdditionalDirectories != nil {
		cfg.AdditionalDirectories = intersectSandboxDirectories(cfg.AdditionalDirectories, settings.AdditionalDirectories)
	}
	if settings.AdditionalReadOnlyDirectories != nil {
		cfg.AdditionalReadOnlyDirectories = intersectSandboxDirectories(cfg.AdditionalReadOnlyDirectories, settings.AdditionalReadOnlyDirectories)
	}
	if settings.DisallowedDirectories != nil {
		cfg.DisallowedDirectories = appendUniqueSandboxDirectories(cfg.DisallowedDirectories, settings.DisallowedDirectories)
	}
	if settings.ContextWindow > 0 && !launchOverride("context_window") {
		cfg.ContextWindow = settings.ContextWindow
	}
	if settings.CompactThreshold > 0 && !launchOverride("compact_threshold") {
		cfg.CompactThreshold = settings.CompactThreshold
	}
	if settings.MaxResultSizeChars > 0 && !launchOverride("max_result_size_chars") {
		cfg.MaxResultSizeChars = settings.MaxResultSizeChars
	}
	if settings.MaxTurns > 0 && !launchOverride("max_turns") {
		cfg.MaxTurns = settings.MaxTurns
	}
	if settings.GenerationOptionsSet {
		if !launchOverride("reasoning_effort") {
			cfg.ReasoningEffort = settings.ReasoningEffort
		}
		if !launchOverride("verbosity") {
			cfg.Verbosity = settings.Verbosity
		}
	}
	if settings.MaxToolOutputCharsPerTurn > 0 && !launchOverride("max_tool_output_chars_per_turn") {
		cfg.MaxToolOutputCharsPerTurn = settings.MaxToolOutputCharsPerTurn
	}
}

func intersectSandboxDirectories(current, saved []string) []string {
	allowed := make(map[string]string, len(current))
	for _, path := range current {
		if canonical := canonicalSandboxDirectory(path); canonical != "" {
			allowed[canonical] = path
		}
	}
	seen := make(map[string]bool, len(saved))
	result := make([]string, 0, len(saved))
	for _, path := range saved {
		canonical := canonicalSandboxDirectory(path)
		if currentPath, ok := allowed[canonical]; ok && !seen[canonical] {
			result = append(result, currentPath)
			seen[canonical] = true
		}
	}
	return result
}

func appendUniqueSandboxDirectories(left, right []string) []string {
	result := append([]string(nil), left...)
	seen := make(map[string]bool, len(left)+len(right))
	for _, path := range left {
		if canonical := canonicalSandboxDirectory(path); canonical != "" {
			seen[canonical] = true
		}
	}
	for _, path := range right {
		canonical := canonicalSandboxDirectory(path)
		if canonical != "" && !seen[canonical] {
			result = append(result, path)
			seen[canonical] = true
		}
	}
	return result
}

func canonicalSandboxDirectory(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// buildEffectiveConfig merges the base config with project settings and, for a
// resume, the durable session settings snapshot, then applies the isolation
// overrides for child/guardian sessions. It returns the validated config the
// runtime is built from.
func buildEffectiveConfig(base config.Config, initial *SessionSnapshot, isolated bool, opts Options) (config.Config, error) {
	trustStore := base.TrustStore
	if trustStore == nil {
		trustStore = config.DefaultTrustStore()
	}
	if !opts.EffectiveConfigFrozen {
		projectCfg, err := config.LoadProjectSettingsTrusted(base.Workspace, trustStore)
		if err != nil {
			return config.Config{}, err
		}
		config.ApplyProjectSettings(&base, &projectCfg)
	}
	base.TrustStore = trustStore
	if initial != nil && initial.Settings != nil {
		applySessionSettingsToConfig(&base, *initial.Settings)
	}
	if !isolated && !opts.EffectiveConfigFrozen {
		if err := base.ApplyProjectPermission(); err != nil {
			return config.Config{}, err
		}
	}
	if isolated {
		// Child and guardian sessions never inherit lifecycle side effects from
		// the parent. Their tool admission is enforced separately by
		// ChildToolGate, but startup hooks/MCP/memory must also be disabled so a
		// child cannot run arbitrary commands merely by being constructed.
		if opts.Purpose == childPurposeTask {
			// Preserve only the parent's prompt/tool decision hooks. The child
			// never receives SessionStart/End or SubagentStart/Stop hooks; those
			// lifecycle events belong to the owning runtime and are not replayed.
			base.Hooks = childDecisionHooks(base.Hooks)
		} else {
			base.Hooks = hooks.Config{}
		}
		base.MCPServers = map[string]mcp.ServerConfig{}
		base.EnableGuardian = config.BoolPtr(false)
		base.EnableMemory = config.BoolPtr(false)
		if opts.Purpose == childPurposeGuardian || opts.Purpose == childPurposeReview {
			// Guardian's read-only gate is name based. Do not let a configured
			// custom command replace a built-in name such as Read and turn that
			// allowlisted name into arbitrary shell execution.
			base.Tools = nil
		}
	}
	if err := base.Validate(); err != nil {
		return config.Config{}, err
	}
	return base, nil
}

// validateResumeSession checks the restored session identity and, for a fresh
// session, guards against clobbering an existing durable log.
func validateResumeSession(cfg *config.Config, initial *SessionSnapshot) error {
	if cfg.SessionID != "" {
		if err := validateSessionID(cfg.SessionID); err != nil {
			return err
		}
	}
	if initial != nil {
		if err := validateSessionID(initial.ID); err != nil {
			return err
		}
		if cfg.SessionID != initial.ID {
			return fmt.Errorf("agent: resume config session id %q does not match snapshot %q", cfg.SessionID, initial.ID)
		}
	}
	if initial == nil && !cfg.NoSessionPersistence && cfg.SessionID != "" {
		existingEvents := filepath.Join(cfg.SessionDir, cfg.SessionID, "events.v1.jsonl")
		if _, err := os.Stat(existingEvents); err == nil {
			return fmt.Errorf("agent: session %q already exists; use Resume", cfg.SessionID)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("agent: inspect session %q: %w", cfg.SessionID, err)
		}
	}
	return nil
}
