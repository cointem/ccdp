package agent

import (
	"errors"
	"time"

	"ccdp/internal/config"

	"ccdp/internal/session/journal"
)

func openSessionPersistence(cfg *config.Config, sessionID string, createdAt time.Time) (*sessionPersistence, error) {
	return openSessionPersistenceMode(cfg, sessionID, createdAt, "agent", false)
}

func openSessionPersistenceMode(cfg *config.Config, sessionID string, createdAt time.Time, source string, rejectExisting bool) (*sessionPersistence, error) {
	return openSessionPersistenceModeWithParent(cfg, sessionID, createdAt, source, rejectExisting, "")
}

func openSessionPersistenceModeWithParent(cfg *config.Config, sessionID string, createdAt time.Time, source string, rejectExisting bool, parentID string, backend ...journal.BackendOptions) (*sessionPersistence, error) {
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	opts := journal.OpenOptions{SessionID: sessionID, SessionDir: cfg.SessionDir, CreatedAt: createdAt, Source: source, ParentID: parentID, Workspace: cfg.Workspace, Model: cfg.Model, NoSessionPersistence: cfg.NoSessionPersistence, RejectExisting: rejectExisting, ProjectMemoryEnabled: cfg.EnableProjectMemory, MemoryReadOnly: cfg.MemoryReadOnly, MemoryHistory: cfg.MemoryHistory}
	if len(backend) > 0 {
		opts.Backend = backend[0]
	}
	w, err := journal.Open(opts)
	if err != nil {
		return nil, err
	}
	return &sessionPersistence{Writer: w}, nil
}
