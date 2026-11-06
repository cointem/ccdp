package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/session/archive"
)

// Resume restores an agent from a previously saved session snapshot.
func Resume(cfg *config.Config, snap *archive.Snapshot) (*Agent, error) {
	return resumeWithOptions(cfg, snap, Options{})
}

func resumeWithOptions(cfg *config.Config, snap *archive.Snapshot, opts Options) (*Agent, error) {
	if cfg == nil || snap == nil {
		return nil, fmt.Errorf("agent: resume config and snapshot are required")
	}
	if err := archive.ValidateID(snap.ID); err != nil {
		return nil, err
	}
	if snap.CreatedAt.IsZero() {
		copy := *snap
		copy.CreatedAt = time.Now().UTC()
		snap = &copy
	}
	if !cfg.NoSessionPersistence {
		if _, err := os.Stat(filepath.Join(cfg.SessionDir, snap.ID, "events.v1.jsonl")); err != nil {
			return nil, fmt.Errorf("agent: resume requires an existing JSONL session: %w", err)
		}
	}
	// Construct every session-scoped component with the restored id from the
	// start. This avoids creating hooks, traces and checkpoint stores under a
	// throwaway generated id before swapping to the snapshot.
	resumeCfg, err := ResolveResumeConfig(cfg, snap)
	if err != nil {
		return nil, err
	}
	// Import complete facts and blobs; a display snapshot cannot preserve
	// input identities, frozen attachments, or request accounting.
	if resumeCfg.NoSessionPersistence && len(snap.Records) > 0 {
		opts.memoryBlobs, err = copyMemoryArtifacts(snap.Records, snap.Artifacts)
		if err != nil {
			return nil, fmt.Errorf("import session %s artifacts: %w", snap.ID, err)
		}
		opts.memoryResume = snap.Records
	}
	opts.EffectiveConfigFrozen = true
	a, err := newAgentWithOptions(&resumeCfg, snap, opts)
	if err != nil {
		return nil, err
	}
	if len(snap.PendingInputs) > 0 {
		pendingCount := len(snap.PendingInputs)
		a.emitStatus("%d queued message(s) restored — they will run after the next turn", pendingCount)
	}

	// Refresh mutable hook state, then re-discover deferred tools recorded in
	// the restored trace.
	a.syncHookContext()
	a.restoreDiscoveredFromStore()
	a.emitSessionLifecycle(true)
	return a, nil
}

// ResolveResumeConfig prepares the effective boundary without starting any
// executable services. Applications can obtain consent before Resume.
func ResolveResumeConfig(cfg *config.Config, snap *archive.Snapshot) (config.Config, error) {
	if cfg == nil || snap == nil {
		return config.Config{}, fmt.Errorf("agent: resume config and snapshot are required")
	}
	resolved := cfg.Clone()
	resolved.SessionID = snap.ID
	if snap.Workspace != "" {
		abs, err := filepath.Abs(snap.Workspace)
		if err != nil {
			return resolved, err
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return resolved, fmt.Errorf("agent: restore workspace is not a directory: %s", abs)
		}
		resolved.Workspace = abs
	}
	if snap.Model != "" && cfg.SourceOf("model") != config.SourceCLI && cfg.SourceOf("model") != config.SourceEnvironment {
		resolved.Model = snap.Model
	}
	return buildEffectiveConfig(resolved, snap, false, Options{})
}
