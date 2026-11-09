package agent

import (
	"context"
	"fmt"
	"os"

	"ccdp/internal/config"
	"ccdp/internal/sandbox"
)

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

// buildSandbox uses the same policy assembly for a candidate workspace and a
// fresh session, including all application control paths.
func buildSandbox(cfg *config.Config, dir string) *sandbox.Sandbox {
	candidate := cfg.Clone()
	candidate.Workspace = dir
	return candidate.Sandbox()
}

// The session owns scratch lifetime, independently of configuration generations.
// Losing these roots makes execution fall back to per-command scratch, which
// disappears before Read (or the next Bash invocation) can use its files.
func rebuildSandbox(cfg *config.Config, dir string, previous *sandbox.Sandbox) *sandbox.Sandbox {
	next := buildSandbox(cfg, dir)
	next.ShareExecutionGuard(previous)
	if previous != nil {
		for _, root := range previous.Snapshot().ArtifactRoots {
			next.AddArtifactRoot(root)
		}
		for _, root := range previous.Snapshot().ScratchDirs {
			next.AddScratchDir(root)
		}
	}
	return next
}
