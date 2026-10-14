package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"ccdp/internal/config"
	"ccdp/internal/sandbox"
)

// SetWorkspace changes the agent's working directory at runtime (Codex /cd),
// rebuilding the sandbox and invalidating the workspace model. The sandbox
// pointer and workspace are swapped atomically under a.mu; tool goroutines
// read them via currentSandbox(), so a /cd during a running turn never gives
// half of one batch the old directory and half the new one.
func (a *Agent) SetWorkspace(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	if _, err := a.reserveSettingsMutation(); err != nil {
		return err
	}
	defer a.releaseSettingsMutation()
	if err := a.setWorkspaceContext(a.lifecycle.rootCtx, abs); err != nil {
		return err
	}
	a.emitStatus("workspace → %s", abs)
	return nil
}

func (a *Agent) setWorkspaceContext(ctx context.Context, abs string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory: %s", abs)
	}
	candidate, err := a.prepareSettingsCandidate(abs)
	if err != nil {
		return err
	}
	candidate.workspaceChange = true
	return a.commitSettingsCandidateContext(ctx, candidate)
}

// buildSandbox assembles the runtime sandbox for dir from config (mirrors
// config.Config.Sandbox but for an arbitrary workspace, without touching the
// config package).
func buildSandbox(cfg *config.Config, dir string) *sandbox.Sandbox {
	s := sandbox.New(dir)
	if cfg.SandboxLimits != nil {
		lim := *cfg.SandboxLimits
		s.Limits = &lim
	}
	s.AllowNetwork = cfg.NetworkAccess
	for _, d := range cfg.AdditionalDirectories {
		s.AddDir(d)
	}
	for _, d := range cfg.AdditionalReadOnlyDirectories {
		s.AddReadOnlyDir(d)
	}
	for _, d := range cfg.DisallowedDirectories {
		s.AddDisallowedDir(d)
	}
	gitRoots := append([]string{s.Workspace}, s.AdditionalDirs...)
	for _, root := range gitRoots {
		for _, path := range sandbox.GitControlPaths(root) {
			if s.InWorkspace(path) {
				s.AddProtectedDir(path)
			}
		}
		for _, path := range sandbox.GitControlEntryPaths(root) {
			s.AddProtectedEntry(path)
		}
	}
	if cfg.SessionDir != "" {
		s.AddDisallowedDir(cfg.SessionDir)
		s.AddProtectedDir(cfg.SessionDir)
	}
	s.AddProtectedDir(config.ProjectSettingsDir(dir))
	if source := cfg.SourcePath(); source != "" {
		s.AddProtectedDir(source)
	}
	if cfg.TrustStore != nil && cfg.TrustStore.Path != "" {
		s.AddProtectedDir(cfg.TrustStore.Path)
	}
	return s
}
