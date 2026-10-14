package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ccdp/internal/config"
)

// Resume restores an agent from a previously saved session snapshot.
func Resume(cfg *config.Config, snap *SessionSnapshot) (*Agent, error) {
	if snap == nil {
		return nil, fmt.Errorf("agent: session snapshot is required")
	}
	if err := validateSessionID(snap.ID); err != nil {
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
	resumeCfg := cloneConfig(cfg)
	resumeCfg.SessionID = snap.ID
	if snap.Workspace != "" {
		abs, err := filepath.Abs(snap.Workspace)
		if err != nil {
			return nil, fmt.Errorf("agent: restore workspace: %w", err)
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("agent: restore workspace is not a directory: %s", snap.Workspace)
		}
		resumeCfg.Workspace = abs
	}
	if snap.Model != "" && cfg.SourceOf("model") != config.SourceCLI && cfg.SourceOf("model") != config.SourceEnvironment {
		resumeCfg.Model = snap.Model
	}
	opts, err := memoryResumeOptions(&resumeCfg, snap)
	if err != nil {
		return nil, err
	}
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
