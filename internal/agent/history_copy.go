package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

// copyHistory materializes source-owned image references before transferring
// history. Destination persistence then creates its own immutable artifacts.
func copyHistory(ctx context.Context, source historyArtifactSource, destination *sessionPersistence, history []messages.Message) ([]messages.Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	copied := messages.Clone(history)
	var sourceBlobs journal.ArtifactReader
	var targetBlobs journal.ArtifactAccess
	for i := range copied {
		for j := range copied[i].ImageAttachments {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			image := &copied[i].ImageAttachments[j]
			if image.BlobHash == "" {
				continue
			}
			if sourceBlobs == nil {
				var err error
				sourceBlobs, err = source.historyArtifacts()
				if err != nil {
					return nil, err
				}
				targetBlobs, err = destination.RequestArtifacts()
				if err != nil {
					return nil, err
				}
			}
			ref := session.BlobRef{Hash: image.BlobHash, Size: image.BlobSize, MediaType: image.MediaType}
			data, err := sourceBlobs.Read(ref, session.DefaultMaxBlobBytes)
			if err != nil {
				return nil, fmt.Errorf("copy history image: %w", err)
			}
			targetRef, err := targetBlobs.PutContext(ctx, data)
			if err != nil {
				return nil, err
			}
			if targetRef.Hash != ref.Hash || targetRef.Size != ref.Size {
				return nil, errors.New("history artifact copy changed identity")
			}
			image.Data = nil
		}
	}
	return copied, nil
}

// stageHistorySession keeps a partially initialized log out of ListSessions.
// The sole publication is the final directory rename after all writes close.
func stageHistorySession(ctx context.Context, cfg config.Config) (config.Config, func() error, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return cfg, nil, nil, err
	}
	if cfg.NoSessionPersistence {
		return cfg, func() error { return nil }, func() {}, nil
	}
	if err := os.MkdirAll(cfg.SessionDir, 0700); err != nil {
		return cfg, nil, nil, err
	}
	staging, err := os.MkdirTemp(cfg.SessionDir, ".history-init-")
	if err != nil {
		return cfg, nil, nil, err
	}
	original := cfg.SessionDir
	cfg.SessionDir = staging
	publish := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(staging, cfg.SessionID)
		target := filepath.Join(original, cfg.SessionID)
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("session %s already exists", cfg.SessionID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(source, target); err != nil {
			return err
		}
		dir, err := os.Open(original)
		if err != nil {
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
	}
	return cfg, publish, func() { _ = os.RemoveAll(staging) }, nil
}

func newForkAgent(cfg *config.Config, opts Options, source historyArtifactSource, history []messages.Message) (*Agent, error) {
	ctx := opts.RootContext
	if ctx == nil {
		ctx = context.Background()
	}
	staged, publish, cleanup, err := stageHistorySession(ctx, cfg.Clone())
	if err != nil {
		return nil, err
	}
	defer cleanup()
	p, err := openSessionPersistenceModeWithParent(&staged, cfg.SessionID, time.Now().UTC(), opts.Purpose, true, string(opts.ParentSessionID))
	if err != nil {
		return nil, err
	}
	defer p.Close()
	copied, err := copyHistory(ctx, source, p, history)
	if err != nil {
		return nil, err
	}
	if err := p.InitializeHistory(ctx, copied, "fork-"+cfg.SessionID); err != nil {
		return nil, err
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return nil, err
	}
	initial, err := snapshotFromRecords(records, cfg.SessionID)
	if err != nil {
		return nil, err
	}
	if cfg.NoSessionPersistence {
		_, blobs, err := p.ReadSnapshot()
		if err != nil {
			return nil, err
		}
		opts.memoryResume, opts.memoryBlobs = records, blobs
	} else {
		if err := p.Close(); err != nil {
			return nil, err
		}
		if err := publish(); err != nil {
			return nil, err
		}
	}
	return newAgentWithOptions(cfg, initial, opts)
}

// Forking only borrows read access to source blobs.
type historyArtifactSource interface {
	historyArtifacts() (journal.ArtifactReader, error)
}

func (p *sessionPersistence) historyArtifacts() (journal.ArtifactReader, error) {
	return p.RequestArtifacts()
}
