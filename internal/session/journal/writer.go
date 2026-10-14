package journal

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/session"
	"ccdp/internal/session/replay"
	"ccdp/internal/sessionview"
)

var sessionLifecycleCounter uint64

// Writer serializes Agent-side commits and keeps the last failure
// visible to the runtime.  Store itself is safe for concurrent calls, but the
// Agent projection is a single-owner stream; this mutex also makes the
// expected-cursor/read/commit sequence atomic for adapter helpers.
type Writer struct {
	memory         bool
	artifactAccess ArtifactAccess
	transcript     *sessionview.Transcript
	mu             sync.Mutex
	store          session.Store
	// confirmedCursor is published only after Commit returns successfully.
	// Read views must never wait behind filesystem I/O while holding a runtime
	// lock needed by cancellation or shutdown.
	confirmedCursor atomic.Uint64

	sessionID string
	dir       string
	failed    error
	// projection is the incrementally maintained typed-fact projection. It
	// intentionally stays in the raw (not synthetic-unknown) form so a later
	// ToolFinished can close a Started call without leaving a duplicate
	// recovery message behind. Read-only snapshot callers finalize a clone.
	projection       *replay.State
	stateStorage     *replay.State
	stateLock        sync.Locker
	projectionCursor session.Cursor
	requests         RequestIndex

	// closeTransactionID and closeAt identify this particular opened Agent
	// lifecycle. A session may be opened and closed repeatedly; using only the
	// session ID would make the later close collide with an earlier durable
	// transaction. Both values are frozen so a retry after an uncertain write
	// submits byte-identical event data with the same transaction ID.
	closeTransactionID string
	closeAt            time.Time
}

// newSessionLifecycleID is intentionally independent of the durable session
// ID. The same session can have multiple simultaneously-created handles over
// its lifetime, while retries on one handle must retain one transaction ID.
func newSessionLifecycleID(sessionID string) string {
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		return "session-closed-" + sessionID + "-" + hex.EncodeToString(random[:])
	}
	sequence := atomic.AddUint64(&sessionLifecycleCounter, 1)
	return fmt.Sprintf("session-closed-%s-%d-%d", sessionID, time.Now().UnixNano(), sequence)
}

func (p *Writer) Dir() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dir
}

func (p *Writer) CurrentCursor() session.Cursor {
	if p == nil {
		return session.Beginning
	}
	return p.confirmedCursor.Load()
}

func (p *Writer) Read(after session.Cursor) ([]session.Record, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return nil, session.ErrClosed
	}
	return p.store.Read(after)
}

// Failure returns the first durable write failure.  Cursor conflicts and
// validation errors are returned by Commit but do not poison the writer;
// ErrPersistenceFailed does, because continuing would make confirmed state
// diverge from the durable log.
func (p *Writer) Failure() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}

func (p *Writer) commitLocked(batch session.Batch) (session.CommitResult, error) {
	if p == nil || p.store == nil {
		return session.CommitResult{}, errors.New("agent: session persistence is not open")
	}
	if p.failed != nil {
		return session.CommitResult{}, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if _, err := p.projectionLocked(); err != nil {
		p.failed = fmt.Errorf("%w: rebuild committed projection: %v", session.ErrPersistenceFailed, err)
		return session.CommitResult{}, p.failed
	}
	result, err := p.store.Commit(p.store.CurrentCursor(), batch)
	if err == nil && result.Applied {
		p.transcript.Facts(batch.Events)
		for _, event := range batch.Events {
			p.requests.Apply(event, true)
		}
	}
	if err != nil && errors.Is(err, session.ErrPersistenceFailed) {
		p.failed = err
	}
	if err == nil && result.Applied && p.projection != nil {
		applyErr := func() error {
			if p.stateLock != nil {
				p.stateLock.Lock()
				defer p.stateLock.Unlock()
			}
			for _, event := range batch.Events {
				if err := p.projection.Apply(session.EventPointer(event)); err != nil {
					return err
				}
			}
			return nil
		}()
		if applyErr != nil {
			p.projection = nil
			p.projectionCursor = session.Beginning
			if _, rebuildErr := p.projectionLocked(); rebuildErr != nil {
				p.failed = fmt.Errorf("%w: committed transaction %q could not be projected: %v (rebuild: %v)", session.ErrPersistenceFailed, batch.TransactionID, applyErr, rebuildErr)
				p.confirmedCursor.Store(p.store.CurrentCursor())
				return result, p.failed
			}
		}
		if p.projection != nil {
			p.projectionCursor = result.Cursor
		}
	}
	if err == nil {
		// A duplicate transaction may return its original, older cursor. The
		// store's current watermark includes all later committed transactions.
		p.confirmedCursor.Store(p.store.CurrentCursor())
	}
	return result, err
}

// Commit appends one transaction at the current durable cursor.  Runtime
// admission must call this before publishing the corresponding accepted
// state or starting an external side effect.
func (p *Writer) Commit(batch session.Batch) (session.CommitResult, error) {
	if p == nil {
		return session.CommitResult{}, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commitLocked(batch)
}

func (p *Writer) CommitEvents(txID string, events ...session.Event) (session.CommitResult, error) {
	if len(events) == 0 {
		return session.CommitResult{Cursor: p.CurrentCursor()}, nil
	}
	return p.Commit(session.Batch{TransactionID: txID, Events: events})
}

func (p *Writer) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return nil
	}
	err := p.store.Close()
	p.store = nil
	p.confirmedCursor.Store(session.Beginning)
	p.artifactAccess = nil
	p.projection = nil
	p.projectionCursor = session.Beginning
	p.requests = RequestIndex{}
	return err
}

func (p *Writer) PutBlob(ctx context.Context, data []byte, mediaType string) (*session.BlobRef, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	ref, err := p.putBlobLocked(ctx, data, mediaType)
	p.mu.Unlock()
	return ref, err
}

// putBlobLocked is used by typed-message commits that already hold p.mu. The
// blob store itself is safe for concurrent use, but keeping the selection of
// disk versus memory artifacts under the persistence lock makes NoSession
// requests and normal session artifacts follow the same transaction path.
func (p *Writer) putBlobLocked(ctx context.Context, data []byte, mediaType string) (*session.BlobRef, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	if p.failed != nil {
		return nil, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	artifacts := p.artifactAccess
	ref, err := artifacts.PutContext(ctx, data)
	if err != nil {
		return nil, err
	}
	ref.MediaType = mediaType
	return &ref, nil
}

// readSnapshot captures records and the matching artifact capability under the
// same lifetime boundary. Consumers cannot close or replace the owned store.
func (p *Writer) ReadSnapshot() ([]session.Record, ArtifactAccess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return nil, nil, session.ErrClosed
	}
	blobs := p.artifactAccess
	records, err := p.store.Read(session.Beginning)
	return records, blobs, err
}

func (p *Writer) TranscriptView() *sessionview.Transcript { return p.transcript }

func (p *Writer) Identity() string { return p.sessionID }

func (p *Writer) PersistClosed(outcome string) error {
	sessionID := p.sessionID
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	if p.failed != nil {
		return fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if p.closeTransactionID == "" {
		p.closeTransactionID = newSessionLifecycleID(sessionID)
	}
	if p.closeAt.IsZero() {
		p.closeAt = time.Now().UTC()
	}
	_, err := p.commitLocked(session.Batch{
		TransactionID: p.closeTransactionID,
		Events:        []session.Event{session.SessionClosed{SessionID: sessionID, Outcome: outcome, ClosedAt: p.closeAt}},
	})
	return err
}

func (p *Writer) RequestArtifacts() (ArtifactAccess, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed != nil {
		return nil, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if p.store == nil {
		return nil, session.ErrClosed
	}
	return p.artifactAccess, nil
}

func (p *Writer) PoisonRequestJournal(err error) {
	if p == nil || err == nil {
		return
	}
	p.mu.Lock()
	if p.failed == nil {
		p.failed = err
	}
	p.mu.Unlock()
}
