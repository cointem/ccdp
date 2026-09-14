package agent

// This file is the Agent-side adapter for the typed session store.  The
// session package owns the log format and the write guarantees; this adapter
// owns only the conversion between the current Agent model and durable facts.
// In particular, it never writes the legacy <id>.json snapshot.

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

const (
	legacyImportSource   = "legacy-json"
	legacyImportBatch    = "legacy-import"
	legacyImportComplete = "legacy-import-complete"
)

var messageIDCounter uint64
var sessionLifecycleCounter uint64

// sessionPersistence serializes Agent-side commits and keeps the last failure
// visible to the runtime.  Store itself is safe for concurrent calls, but the
// Agent projection is a single-owner stream; this mutex also makes the
// expected-cursor/read/commit sequence atomic for adapter helpers.
type sessionPersistence struct {
	transcript      *transcriptState
	mu              sync.Mutex
	store           session.Store
	artifacts       *session.ArtifactStore
	memoryArtifacts *memoryRequestArtifacts
	sessionID       string
	dir             string
	failed          error
	// projection is the incrementally maintained typed-fact projection. It
	// intentionally stays in the raw (not synthetic-unknown) form so a later
	// ToolFinished can close a Started call without leaving a duplicate
	// recovery message behind. Read-only snapshot callers finalize a clone.
	projection       *replayProjection
	projectionCursor session.Cursor
	// closeTransactionID and closeAt identify this particular opened Agent
	// lifecycle. A session may be opened and closed repeatedly; using only the
	// session ID would make the later close collide with an earlier durable
	// transaction. Both values are frozen so a retry after an uncertain write
	// submits byte-identical event data with the same transaction ID.
	closeTransactionID string
	closeAt            time.Time
}

func (a *Agent) openPersistence() error {
	return a.openPersistenceMode(false)
}

func (a *Agent) openPersistenceFresh() error {
	return a.openPersistenceMode(true)
}

func (a *Agent) openPersistenceMode(rejectExisting bool) error {
	if a == nil || a.cfg == nil {
		return errors.New("agent: persistence requires an initialized agent")
	}
	a.mu.Lock()
	parentID := a.parentID
	source := "agent"
	if a.childState != nil && (a.childState.purpose == childPurposeTask || a.childState.purpose == childPurposeGuardian) {
		source = a.childState.purpose
	}
	a.mu.Unlock()
	p, err := openSessionPersistenceModeWithParent(a.cfg, a.sessionID, a.createdAt, source, rejectExisting, parentID)
	if err != nil {
		a.mu.Lock()
		a.persistenceErr = err
		a.mu.Unlock()
		return err
	}
	a.mu.Lock()
	a.persistence = p
	a.transcript = p.transcript
	a.persistenceErr = nil
	a.mu.Unlock()
	return nil
}

func (a *Agent) closePersistence() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	p := a.persistence
	a.persistence = nil
	a.mu.Unlock()
	if p == nil {
		return nil
	}
	err := p.close()
	if err != nil {
		a.mu.Lock()
		a.persistenceErr = err
		a.mu.Unlock()
	}
	return err
}

// persistSessionClosed records the lifecycle terminal fact before the store is
// released. Close is already past all turn/operation joins here, so this fact
// cannot race a producer and a replay can distinguish a clean close from a
// crash after SessionCreated.
func (a *Agent) persistSessionClosed(outcome string) error {
	if a == nil {
		return nil
	}
	if outcome == "" {
		outcome = "success"
	}
	p := a.persistenceHandle()
	if p == nil {
		return nil
	}
	a.mu.Lock()
	sessionID := a.sessionID
	a.mu.Unlock()
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

func (a *Agent) persistenceHandle() *sessionPersistence {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.persistence
}

// persistenceFailure reports a poisoned durable writer. Runtime admission
// uses this gate to stop accepting work after a synced commit failed; callers
// must not continue to start a turn against an uncertain log.
func (a *Agent) persistenceFailure() error {
	if a == nil {
		return errors.New("agent: nil agent")
	}
	a.mu.Lock()
	err := a.persistenceErr
	p := a.persistence
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if p != nil {
		return p.Failure()
	}
	return nil
}

func ensureMessageID(message *messages.Message) {
	if message == nil || message.ID != "" {
		return
	}
	// Runtime messages receive an identity once, at admission to Agent history.
	// It must not be content-derived: two identical messages are still distinct
	// conversation entries. The identity is then carried by Session.Message and
	// retained verbatim during replay.
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		message.ID = "message-" + hex.EncodeToString(random[:])
		return
	}
	sequence := atomic.AddUint64(&messageIDCounter, 1)
	message.ID = fmt.Sprintf("message-%d-%d", time.Now().UnixNano(), sequence)
}

func inputMessageID(inputID string) string {
	return stableID("input-message", inputID)
}

func legacyMessageID(sessionID string, index int) string {
	return stableID("legacy-message", struct {
		SessionID string
		Index     int
	}{SessionID: sessionID, Index: index})
}

// persistInput queues a durable inbox fact.  Runtime admission must call it
// before mutating pendingMsgs or starting a turn.
func (a *Agent) persistInput(inputID, text string, strategy string, turnID string, createdAt time.Time, attachments ...messages.ImageAttachment) error {
	return a.persistInputWithDigest(inputID, text, strategy, turnID, createdAt, "", attachments...)
}

func (a *Agent) persistInputWithDigest(inputID, text string, strategy string, turnID string, createdAt time.Time, digest string, attachments ...messages.ImageAttachment) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.persistInputQueuedWithDigest(inputID, text, strategy, turnID, createdAt, digest, attachments...)
}

func (a *Agent) persistInputDelivered(inputID, turnID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.persistInputDelivered(inputID, turnID)
}

func (a *Agent) persistHistory(history []messages.Message, turnID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	return p.persistHistoryMessages(history, turnID)
}

// restoreInputDedup rebuilds command-level input identity from durable queue
// facts. A restarted runtime must not execute a previously delivered input a
// second time merely because its in-memory seenInputs map was empty.
func (a *Agent) restoreInputDedup() error {
	if a == nil {
		return errors.New("agent: nil agent")
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return err
	}
	type durableInput struct {
		digest    string
		body      protocol.SubmitInput
		delivered bool
		cancelled bool
	}
	inputs := make(map[protocol.InputID]*durableInput)
	for _, record := range records {
		switch event := record.Event.(type) {
		case *session.InputQueued:
			strategy := protocol.InputStrategy(event.Strategy)
			if strategy != protocol.InputSteer && strategy != protocol.InputFollowup {
				strategy = protocol.InputFollowup
			}
			id := protocol.InputID(event.InputID)
			inputs[id] = &durableInput{body: protocol.SubmitInput{ID: id, Text: event.Text, Strategy: strategy}, digest: event.InputDigest}
		case *session.InputDelivered:
			if input := inputs[protocol.InputID(event.InputID)]; input != nil {
				input.delivered = true
			}
		case *session.InputCancelled:
			if input := inputs[protocol.InputID(event.InputID)]; input != nil {
				input.cancelled = true
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, input := range inputs {
		if id == "" || input.cancelled {
			continue
		}
		status := protocol.ReceiptScheduled
		if input.delivered {
			status = protocol.ReceiptApplied
		}
		a.seenInputs[id] = protocol.Receipt{CommandID: protocol.CommandID(id), SessionID: protocol.SessionID(a.sessionID), Status: status, OperationID: protocol.OperationID("resume-input-" + string(id))}
		digest, digestErr := submitInputDigest(input.body)
		if digestErr != nil {
			return fmt.Errorf("encode resumed input identity: %w", digestErr)
		}
		a.seenInputBody[id] = digest
		if input.digest != "" {
			a.seenInputBody[id] = input.digest
		}
	}
	return nil
}

func openSessionPersistence(cfg *config.Config, sessionID string, createdAt time.Time) (*sessionPersistence, error) {
	return openSessionPersistenceMode(cfg, sessionID, createdAt, "agent", false)
}

func openSessionPersistenceSource(cfg *config.Config, sessionID string, createdAt time.Time, source string) (*sessionPersistence, error) {
	return openSessionPersistenceMode(cfg, sessionID, createdAt, source, false)
}

func openSessionPersistenceMode(cfg *config.Config, sessionID string, createdAt time.Time, source string, rejectExisting bool) (*sessionPersistence, error) {
	return openSessionPersistenceModeWithParent(cfg, sessionID, createdAt, source, rejectExisting, "")
}

func openSessionPersistenceModeWithParent(cfg *config.Config, sessionID string, createdAt time.Time, source string, rejectExisting bool, parentID string) (*sessionPersistence, error) {
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if parentID != "" {
		if err := validateSessionID(parentID); err != nil {
			return nil, fmt.Errorf("agent: invalid parent session id: %w", err)
		}
	}

	p := &sessionPersistence{sessionID: sessionID, closeTransactionID: newSessionLifecycleID(sessionID), transcript: &transcriptState{}}
	if cfg.NoSessionPersistence {
		p.store = session.NewMemoryStore()
		// Memory mode still has to retain the complete prepared wire body for
		// the lifetime of this Agent. A nil ArtifactStore is not permission to
		// drop request bytes or silently fall back to the session directory.
		p.memoryArtifacts = newMemoryRequestArtifacts(session.DefaultMaxBlobBytes)
	} else {
		store, err := session.OpenJSONLStore(cfg.SessionDir, sessionID)
		if err != nil {
			return nil, err
		}
		p.store = store
		p.dir = store.Dir()
		artifacts, err := session.NewArtifactStore(store.Dir())
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("agent: open session artifacts: %w", err)
		}
		p.artifacts = artifacts
	}

	if p.store.CurrentCursor() != session.Beginning && rejectExisting {
		_ = p.close()
		return nil, fmt.Errorf("agent: session %q already exists; use Resume", sessionID)
	}
	if p.store.CurrentCursor() == session.Beginning {
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		initialEvents := []session.Event{session.SessionCreated{
			SessionID:     sessionID,
			FormatVersion: session.SchemaVersion,
			Source:        source,
			ParentID:      parentID,
			CreatedAt:     createdAt,
		}}
		if cfg.Workspace != "" || cfg.Model != "" {
			initialEvents = append(initialEvents, session.SettingsChanged{Revision: 1, Settings: session.Settings{
				Workspace: cfg.Workspace,
				Model:     cfg.Model,
			}})
		}
		_, err := p.commitLocked(session.Batch{
			TransactionID: "session-created-" + sessionID,
			Events:        initialEvents,
		})
		if err != nil {
			_ = p.store.Close()
			return nil, fmt.Errorf("agent: persist session creation: %w", err)
		}
	}
	records, readErr := p.store.Read(session.Beginning)
	if readErr != nil {
		_ = p.close()
		return nil, readErr
	}
	p.transcript = transcriptFromRecords(records)
	return p, nil
}

func (p *sessionPersistence) Store() session.Store {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store
}

func (p *sessionPersistence) Artifacts() *session.ArtifactStore {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.artifacts
}

func (p *sessionPersistence) Dir() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dir
}

func (p *sessionPersistence) CurrentCursor() session.Cursor {
	if p == nil {
		return session.Beginning
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.Beginning
	}
	return p.store.CurrentCursor()
}

func (p *sessionPersistence) Read(after session.Cursor) ([]session.Record, error) {
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
func (p *sessionPersistence) Failure() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}

func (p *sessionPersistence) commitLocked(batch session.Batch) (session.CommitResult, error) {
	if p == nil || p.store == nil {
		return session.CommitResult{}, errors.New("agent: session persistence is not open")
	}
	if p.failed != nil {
		return session.CommitResult{}, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	result, err := p.store.Commit(p.store.CurrentCursor(), batch)
	if err == nil && result.Applied {
		p.transcript.facts(batch.Events)
	}
	if err != nil && errors.Is(err, session.ErrPersistenceFailed) {
		p.failed = err
	}
	if err == nil && result.Applied && p.projection != nil {
		for _, event := range batch.Events {
			if applyErr := p.projection.apply(projectionEvent(event)); applyErr != nil {
				// The Store validates before commit, so this is an adapter bug or
				// an impossible legacy value. Do not continue with a cache that
				// disagrees with the authoritative log; force the next read to
				// rebuild instead.
				p.projection = nil
				p.projectionCursor = session.Beginning
				break
			}
		}
		if p.projection != nil {
			p.projectionCursor = result.Cursor
		}
	}
	return result, err
}

// projectionEvent gives the replay reducer the same pointer-shaped values it
// receives from JSONL/Memory Read. Commit callers commonly pass value events;
// normalizing here keeps the incremental cache complete without another full
// log scan after every fact.
func projectionEvent(event session.Event) session.Event {
	switch e := event.(type) {
	case session.SessionCreated:
		v := e
		return &v
	case session.SessionImported:
		v := e
		return &v
	case session.SessionClosed:
		v := e
		return &v
	case session.SettingsChanged:
		v := e
		return &v
	case session.SettingsScheduled:
		v := e
		return &v
	case session.SettingsScheduleCancelled:
		v := e
		return &v
	case session.WorkflowChanged:
		v := e
		return &v
	case session.TurnStarted:
		v := e
		return &v
	case session.TurnFinished:
		v := e
		return &v
	case session.RequestPrepared:
		v := e
		return &v
	case session.CommandCompleted:
		v := e
		return &v
	case session.InputQueued:
		v := e
		return &v
	case session.InputDelivered:
		v := e
		return &v
	case session.InputCancelled:
		v := e
		return &v
	case session.CommandScheduled:
		v := e
		return &v
	case session.AssistantCommitted:
		v := e
		return &v
	case session.ToolStarted:
		v := e
		return &v
	case session.ToolFinished:
		v := e
		return &v
	case session.ToolResultsProjected:
		v := e
		return &v
	case session.ContextCompacted:
		v := e
		return &v
	case session.ConversationReset:
		v := e
		return &v
	case session.UsageChanged:
		v := e
		return &v
	case session.AttemptFinished:
		v := e
		return &v
	case session.ApprovalRequested:
		v := e
		return &v
	case session.ApprovalResolved:
		v := e
		return &v
	case session.HookStarted:
		v := e
		return &v
	case session.HookFinished:
		v := e
		return &v
	case session.ConversationRewound:
		v := e
		return &v
	case session.TasksChanged:
		v := e
		return &v
	case session.MemoryChanged:
		v := e
		return &v
	case session.ToolsDiscovered:
		v := e
		return &v
	default:
		return event
	}
}

// Commit appends one transaction at the current durable cursor.  Runtime
// admission must call this before publishing the corresponding accepted
// state or starting an external side effect.
func (p *sessionPersistence) Commit(batch session.Batch) (session.CommitResult, error) {
	if p == nil {
		return session.CommitResult{}, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commitLocked(batch)
}

func (p *sessionPersistence) commitEvents(txID string, events ...session.Event) (session.CommitResult, error) {
	if len(events) == 0 {
		return session.CommitResult{Cursor: p.CurrentCursor()}, nil
	}
	return p.Commit(session.Batch{TransactionID: txID, Events: events})
}

func (p *sessionPersistence) close() error {
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
	p.artifacts = nil
	p.memoryArtifacts = nil
	p.projection = nil
	p.projectionCursor = session.Beginning
	return err
}

// saveProjection writes only the optional acceleration cache.  The event log
// remains authoritative and can rebuild this exact value after deletion.
func (p *sessionPersistence) saveProjection(snapshot SessionSnapshot) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed != nil {
		return fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if p.store == nil {
		return session.ErrClosed
	}
	err = p.store.SaveSnapshot(session.Snapshot{
		SchemaVersion: session.SchemaVersion,
		SessionID:     snapshot.ID,
		LastSeq:       p.store.CurrentCursor(),
		UpdatedAt:     snapshot.UpdatedAt,
		Data:          data,
	})
	if err != nil && errors.Is(err, session.ErrPersistenceFailed) {
		p.failed = err
	}
	return err
}

func (p *sessionPersistence) putBlob(ctx context.Context, data []byte, mediaType string) (*session.BlobRef, error) {
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
func (p *sessionPersistence) putBlobLocked(ctx context.Context, data []byte, mediaType string) (*session.BlobRef, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	if p.failed != nil {
		return nil, fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	var artifacts requestBlobStore
	if p.artifacts != nil {
		artifacts = p.artifacts
	} else {
		if p.memoryArtifacts == nil {
			p.memoryArtifacts = newMemoryRequestArtifacts(session.DefaultMaxBlobBytes)
		}
		artifacts = p.memoryArtifacts
	}
	ref, err := artifacts.PutContext(ctx, data)
	if err != nil {
		return nil, err
	}
	ref.MediaType = mediaType
	return &ref, nil
}

// persistInputQueued is the runtime's durable inbox admission helper.  It
// deliberately has no side effects beyond the synced Store commit.
func (p *sessionPersistence) persistInputQueued(inputID, text string, strategy string, turnID string, createdAt time.Time, attachments ...messages.ImageAttachment) error {
	return p.persistInputQueuedWithDigest(inputID, text, strategy, turnID, createdAt, "", attachments...)
}

func (p *sessionPersistence) persistInputQueuedWithDigest(inputID, text string, strategy string, turnID string, createdAt time.Time, digest string, attachments ...messages.ImageAttachment) error {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	refs := make([]session.BlobRef, 0, len(attachments))
	for i := range attachments {
		if attachments[i].Data == nil && attachments[i].BlobHash == "" {
			continue
		}
		if attachments[i].BlobHash != "" {
			ref := session.BlobRef{Hash: attachments[i].BlobHash, Size: attachments[i].BlobSize, MediaType: attachments[i].MediaType}
			if err := validateRequestBlobRef(ref); err != nil {
				return err
			}
			refs = append(refs, ref)
			continue
		}
		ref, err := p.putBlob(context.Background(), attachments[i].Data, attachments[i].MediaType)
		if err != nil {
			return err
		}
		if ref == nil {
			return errors.New("agent: input image artifact was not stored")
		}
		attachments[i].BlobHash, attachments[i].BlobSize = ref.Hash, ref.Size
		attachments[i].MediaType = ref.MediaType
		refs = append(refs, *ref)
	}
	_, err := p.commitEvents("input-queued-"+inputID, session.InputQueued{
		InputDigest: digest,
		InputID:     inputID, MessageID: inputMessageID(inputID), Text: text, Attachments: refs, ImagesFrozen: attachments != nil, Strategy: strategy, TurnID: turnID, CreatedAt: createdAt,
	})
	return err
}

func (p *sessionPersistence) persistInputDelivered(inputID, turnID string) error {
	_, err := p.commitEvents("input-delivered-"+inputID, session.InputDelivered{InputID: inputID, TurnID: turnID})
	return err
}

// persistSettingsSnapshot appends the complete public settings view captured
// at a command/step boundary, so replay never has to guess the active mode,
// sandbox policy, or pending-public binding from mutable Agent state.
func (p *sessionPersistence) persistSettingsSnapshot(settings session.Settings, revision uint64) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistSettingsSnapshotLocked(settings, revision)
}

func (p *sessionPersistence) persistSettingsSnapshotLocked(settings session.Settings, revision uint64) error {
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Settings, settings) && (revision == 0 || revision <= projection.SettingsRev) {
		return nil
	}
	if revision == 0 {
		revision = projection.SettingsRev + 1
		if revision == 0 {
			revision = 1
		}
	}
	baseCursor := p.store.CurrentCursor()
	_, err = p.commitLocked(session.Batch{TransactionID: stableID("settings-snapshot", struct {
		Settings session.Settings
		Revision uint64
		Cursor   session.Cursor
	}{settings, revision, baseCursor}), Events: []session.Event{session.SettingsChanged{Revision: revision, Settings: settings}}})
	return err
}

// persistWorkflow appends a recoverable workflow state transition. Workflow
// is UI/session state, not model history, so replay keeps it in the typed
// projection without manufacturing a chat message.
func (p *sessionPersistence) persistWorkflow(workflow session.WorkflowState) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Workflow, workflow) {
		return nil
	}
	_, err = p.commitLocked(session.Batch{TransactionID: stableID("workflow", struct {
		Workflow session.WorkflowState
		Cursor   session.Cursor
	}{workflow, p.store.CurrentCursor()}), Events: []session.Event{session.WorkflowChanged{Workflow: workflow}}})
	return err
}

// persistMemory stores the bounded, complete memory projection as a typed
// fact. clear is explicit because an empty MemoryChanged payload is invalid;
// no-session mode therefore remains wholly in MemoryStore/artifacts and never
// falls back to the configured session directory.
func (p *sessionPersistence) persistMemory(text string, clear bool) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if projection.memorySeen && ((clear && projection.MemoryText == "") || (!clear && projection.MemoryText == text)) {
		return nil
	}
	event := session.MemoryChanged{Revision: p.store.CurrentCursor() + 1, Text: text, Cleared: clear}
	_, err = p.commitLocked(session.Batch{TransactionID: stableID("memory", struct {
		Text   string
		Clear  bool
		Cursor session.Cursor
	}{text, clear, p.store.CurrentCursor()}), Events: []session.Event{event}})
	return err
}

// persistTasks records the full task-list replacement after TodoWrite has
// durably changed its resource. The typed event is the recovery authority;
// todos.json remains only a compatibility/cache representation.
func (p *sessionPersistence) persistTasks(tasks []session.Task) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Tasks, tasks) {
		return nil
	}
	copyTasks := append([]session.Task(nil), tasks...)
	_, err = p.commitLocked(session.Batch{TransactionID: stableID("tasks", struct {
		Tasks  []session.Task
		Cursor session.Cursor
	}{copyTasks, p.store.CurrentCursor()}), Events: []session.Event{session.TasksChanged{Revision: p.store.CurrentCursor() + 1, Tasks: copyTasks}}})
	return err
}

func (p *sessionPersistence) persistToolsDiscovered(version uint64, schemas []session.ToolSchema) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if version == 0 {
		version = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	allKnown := len(schemas) > 0
	for _, schema := range schemas {
		if _, ok := projection.Discovered[schema.ToolID]; !ok {
			allKnown = false
			break
		}
	}
	if allKnown {
		return nil
	}
	copySchemas := append([]session.ToolSchema(nil), schemas...)
	_, err = p.commitLocked(session.Batch{TransactionID: stableID("tools-discovered", struct {
		Version uint64
		Tools   []session.ToolSchema
		Cursor  session.Cursor
	}{version, copySchemas, p.store.CurrentCursor()}), Events: []session.Event{session.ToolsDiscovered{CatalogVersion: version, Tools: copySchemas}}})
	return err
}

func (p *sessionPersistence) persistCommandCompleted(event session.CommandCompleted) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if event.CommandID == "" {
		return errors.New("agent: command completion id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	_, err := p.commitLocked(session.Batch{TransactionID: stableID("command-completed", event), Events: []session.Event{event}})
	return err
}

// commandInputDigest returns the stable identity used by CommandScheduled.
// It intentionally hashes the full normalized command body (including its
// payload), rather than a display name or operation ID.  The command body is
// never written to the session log, but the digest makes a resumed retry
// distinguish the original request from an ID collision.
func commandInputDigest(cmd protocol.Command) (string, error) {
	body, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// submitInputDigest uses the same canonical JSON encoding as command
// idempotency but excludes the command/session envelope. Input IDs are already
// the map key, so only the normalized input body needs to be retained for a
// live duplicate check. The submitted text therefore never remains in
// seenInputBody after admission.
func submitInputDigest(input protocol.SubmitInput) (string, error) {
	return commandInputDigest(protocol.Command{Type: protocol.CommandSubmitInput, Input: &input})
}

// persistCommandScheduled admits an asynchronous command before its worker is
// started. The digest is over the normalized protocol command, not a display
// label, so replay and Store deduplication can reject a reused ID with a
// different body without putting the command payload into the session log.
func (a *Agent) persistCommandScheduled(cmd protocol.Command, name string, sourceRevision uint64) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	digest, err := commandInputDigest(cmd)
	if err != nil {
		return fmt.Errorf("encode command admission: %w", err)
	}
	event := session.CommandScheduled{CommandID: string(cmd.ID), OperationID: string(cmd.ID), Name: name, InputDigest: digest, SourceRevision: sourceRevision}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.store == nil {
		err = session.ErrClosed
	} else {
		_, err = p.commitLocked(session.Batch{TransactionID: stableID("command-scheduled", event), Events: []session.Event{event}})
	}
	p.mu.Unlock()
	if err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command admission: %w", err))
	}
	return nil
}

// durableCommandState is read directly from the incremental raw projection.
// A scheduled command with no completion is an admitted operation whose
// external outcome is unknown; a completed command is terminal and can be
// replayed as an idempotent receipt.  The command payload itself is never
// recovered from the log.
type durableCommandState struct {
	scheduled      *session.CommandScheduled
	completed      *session.CommandCompleted
	digestMismatch bool
}

func (a *Agent) durableCommandState(cmd protocol.Command) (durableCommandState, error) {
	p := a.persistenceHandle()
	if p == nil {
		return durableCommandState{}, nil
	}
	digest, err := commandInputDigest(cmd)
	if err != nil {
		return durableCommandState{}, fmt.Errorf("encode command identity: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	projection, err := p.projectionLocked()
	if err != nil {
		return durableCommandState{}, err
	}
	id := string(cmd.ID)
	state := durableCommandState{}
	if event, ok := projection.ScheduledCommands[id]; ok {
		copy := event
		state.scheduled = &copy
		state.digestMismatch = event.InputDigest != digest
		return state, nil
	}
	if event, ok := projection.CompletedCommands[id]; ok {
		copy := event
		state.completed = &copy
	}
	return state, nil
}

// persistCommandCompletedWithOutput stores the bounded command output as a
// session artifact and commits its reference with the terminal fact. Blob
// publication and the event share the persistence mutex, so replay never
// observes a successful completion pointing at an unpublished output.
func (a *Agent) persistCommandCompletedWithOutput(event session.CommandCompleted, output string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	var err error
	p.mu.Lock()
	if p.store == nil {
		err = session.ErrClosed
	} else {
		if output != "" {
			ref, blobErr := p.putBlobLocked(context.Background(), []byte(output), "text/plain; charset=utf-8")
			if blobErr != nil {
				err = blobErr
			} else {
				event.Output = ref
			}
		}
		if err == nil {
			_, err = p.commitLocked(session.Batch{TransactionID: stableID("command-completed", event), Events: []session.Event{event}})
		}
	}
	p.mu.Unlock()
	if err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command completion: %w", err))
	}
	return nil
}

func (p *sessionPersistence) memoryText() (string, bool, error) {
	if p == nil {
		return "", false, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	projection, err := p.projectionLocked()
	if err != nil {
		return "", false, err
	}
	return projection.MemoryText, projection.memorySeen, nil
}

// persistSettingsFact is the narrow Agent-facing boundary for command and
// provider code that changes public session settings. It serializes with
// history/tool facts and never accepts credentials (session.Settings has no
// API-key or Authorization fields).
func (a *Agent) persistSettingsFact(settings session.Settings, revision uint64) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := p.persistSettingsSnapshot(settings, revision); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist settings: %w", err))
	}
	return nil
}

// persistSettingsScheduledFact records a candidate that was accepted while a
// turn is still running. The active in-memory binding remains unchanged until
// the next step boundary; replay can therefore distinguish an admitted
// pending change from the setting that was actually used by the current step.
func (a *Agent) persistSettingsScheduledFact(changeID string, settings session.Settings, sourceRevision uint64) error {
	return a.persistSettingsScheduledFactWithCancel(changeID, settings, sourceRevision, "")
}

// persistSettingsScheduledFactWithCancel records a replacement and its
// supersession in one transaction.  A pending model command is an admitted
// operation, so replacing it must close the old command explicitly instead of
// leaving a second "unknown" operation in the resume projection.
func (a *Agent) persistSettingsScheduledFactWithCancel(changeID string, settings session.Settings, sourceRevision uint64, supersededChangeID string) error {
	if strings.TrimSpace(changeID) == "" {
		changeID = nextRuntimeID("settings-change")
	}
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	events := make([]session.Event, 0, 3)
	if supersededChangeID != "" && supersededChangeID != changeID {
		events = append(events,
			session.SettingsScheduleCancelled{ChangeID: supersededChangeID, Reason: "superseded by a newer settings change"},
			session.CommandCompleted{CommandID: supersededChangeID, Outcome: "cancelled", Code: "superseded", Report: "settings change was superseded before its step boundary"},
		)
	}
	events = append(events, session.SettingsScheduled{ChangeID: changeID, Settings: settings, SourceRevision: sourceRevision})
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("settings-scheduled", struct {
			ChangeID       string
			Settings       session.Settings
			SourceRevision uint64
			Superseded     string
		}{changeID, settings, sourceRevision, supersededChangeID}),
		Events: events,
	}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist scheduled settings: %w", err))
	}
	return nil
}

// persistScheduledSettingsAppliedFact closes one durable settings admission at
// the step boundary. SettingsChanged and the terminal CommandCompleted for the
// original scheduled command share a transaction, so replay cannot expose an
// active binding without its completion (or vice versa).
func (a *Agent) persistScheduledSettingsAppliedFact(changeID string, settings session.Settings, revision uint64) error {
	return a.persistSettingsAppliedFact(changeID, settings, revision, "")
}

// persistSettingsAppliedFact commits the active settings snapshot and the
// terminal state of the command which caused it in one transaction.  When a
// stale scheduled candidate is replaced while the session is idle, the
// cancellation of that candidate belongs to this same publication boundary;
// otherwise replay can retain an admitted change whose in-memory receipt was
// already superseded.
func (a *Agent) persistSettingsAppliedFact(changeID string, settings session.Settings, revision uint64, supersededChangeID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.persistMu.Lock()
	var err error
	p.mu.Lock()
	if p.store == nil {
		err = session.ErrClosed
	} else if p.failed != nil {
		err = fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	} else {
		events := make([]session.Event, 0, 4)
		if strings.TrimSpace(supersededChangeID) != "" && supersededChangeID != changeID {
			events = append(events,
				session.SettingsScheduleCancelled{ChangeID: supersededChangeID, Reason: "superseded by a newer settings change"},
				session.CommandCompleted{CommandID: supersededChangeID, Outcome: "cancelled", Code: "superseded", Report: "settings change was superseded before its step boundary"},
			)
		}
		events = append(events, session.SettingsChanged{Revision: revision, Settings: settings})
		if strings.TrimSpace(changeID) != "" {
			events = append(events, session.CommandCompleted{CommandID: changeID, Outcome: "applied"})
		}
		_, err = p.commitLocked(session.Batch{TransactionID: stableID("settings-applied", struct {
			ChangeID   string
			Revision   uint64
			Settings   session.Settings
			Superseded string
		}{changeID, revision, settings, supersededChangeID}), Events: events})
	}
	p.mu.Unlock()
	a.persistMu.Unlock()
	if err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist applied settings: %w", err))
	}
	return nil
}

func (a *Agent) sessionSettingsLocked() session.Settings {
	settings := session.Settings{}
	if a == nil {
		return settings
	}
	settings.Model = a.activeBinding.model
	if settings.Model == "" {
		settings.Model = a.cfg.Model
	}
	settings.Provider = a.activeBinding.provider
	if settings.Provider == "" && a.client != nil {
		settings.Provider = a.client.Name()
	}
	settings.Endpoint = a.activeBinding.endpoint
	if settings.Endpoint == "" {
		settings.Endpoint = a.cfg.BaseURL
	}
	if endpoint, err := sanitizeRequestEndpoint(settings.Endpoint); err == nil {
		settings.Endpoint = endpoint
	} else {
		settings.Endpoint = ""
	}
	settings.Workspace = a.cfg.Workspace
	settings.PermissionPolicy = a.cfg.PermissionMode
	settings.AlwaysAllow = append([]string(nil), a.cfg.AlwaysAllow...)
	settings.AlwaysDeny = append([]string(nil), a.cfg.AlwaysDeny...)
	settings.SandboxPolicy = a.cfg.SandboxMode
	settings.AllowNetwork = a.cfg.SandboxAllowNetwork
	settings.AllowNetworkSet = true
	settings.AdditionalDirectories = append([]string(nil), a.cfg.AdditionalDirectories...)
	settings.DisallowedDirectories = append([]string(nil), a.cfg.DisallowedDirectories...)
	settings.ContextWindow = a.cfg.ContextWindow
	settings.CompactThreshold = a.cfg.CompactThreshold
	settings.MaxResultSizeChars = a.cfg.MaxResultSizeChars
	settings.MaxTurns = a.cfg.MaxTurns
	settings.MaxBudgetUSD = a.cfg.MaxBudgetUSD
	settings.MaxReplyTokens = a.cfg.MaxReplyTokens
	settings.ReasoningEffort = a.cfg.ReasoningEffort
	settings.Verbosity = a.cfg.Verbosity
	settings.GenerationOptionsSet = true
	settings.MaxToolOutputCharsPerTurn = a.cfg.MaxToolOutputCharsPerTurn
	if a.planMode {
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	p.mu.Lock()
	if p.store == nil {
		p.mu.Unlock()
		return a.toolJournalFailure(session.ErrClosed)
	}
	if p.failed != nil {
		err := p.failed
		p.mu.Unlock()
		return err
	}
	projection, err := p.projectionLocked()
	if err != nil {
		p.mu.Unlock()
		return a.toolJournalFailure(fmt.Errorf("agent: inspect settings projection: %w", err))
	}
	events := make([]session.Event, 0, 2)
	if !reflect.DeepEqual(projection.Settings, settings) || revision > projection.SettingsRev {
		if revision == 0 {
			revision = projection.SettingsRev + 1
			if revision == 0 {
				revision = 1
			}
		}
		events = append(events, session.SettingsChanged{Revision: revision, Settings: settings})
	}
	if workflow != nil && *workflow != projection.Workflow {
		events = append(events, session.WorkflowChanged{Workflow: *workflow})
	}
	if len(events) == 0 {
		p.mu.Unlock()
		return nil
	}
	if _, err = p.commitLocked(session.Batch{TransactionID: stableID("settings-workflow", struct {
		Settings session.Settings
		Revision uint64
		Workflow *session.WorkflowState
		Cursor   session.Cursor
	}{settings, revision, workflow, p.store.CurrentCursor()}), Events: events}); err != nil {
		p.mu.Unlock()
		return a.toolJournalFailure(fmt.Errorf("agent: persist settings/workflow: %w", err))
	}
	p.mu.Unlock()
	return nil
}

func (a *Agent) persistWorkflowFact(workflow session.WorkflowState) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := p.persistWorkflow(workflow); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist workflow: %w", err))
	}
	return nil
}

func (a *Agent) persistCommandCompletedFact(event session.CommandCompleted) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := p.persistCommandCompleted(event); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command completion: %w", err))
	}
	return nil
}

func (p *sessionPersistence) persistUsage(usage Usage) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return err
	}
	var last *session.UsageChanged
	for _, record := range records {
		if event, ok := record.Event.(*session.UsageChanged); ok {
			copy := *event
			last = &copy
		}
	}
	converted := session.Usage{InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens), CachedTokens: int64(usage.CachedTokens), TotalTokens: int64(usage.InputTokens + usage.OutputTokens), Cost: usage.Cost, TurnCount: int64(usage.TurnCount)}
	if last != nil && last.Usage == converted {
		return nil
	}
	if usage == (Usage{}) {
		return nil
	}
	baseCursor := p.store.CurrentCursor()
	_, err = p.commitLocked(session.Batch{TransactionID: "usage-" + stableID("usage", struct {
		Usage  session.Usage
		Cursor session.Cursor
	}{converted, baseCursor}), Events: []session.Event{session.UsageChanged{Revision: uint64(len(records) + 1), Usage: converted}}})
	return err
}

func (p *sessionPersistence) projectionLocked() (*replayProjection, error) {
	if p == nil || p.store == nil {
		return nil, session.ErrClosed
	}
	cursor := p.store.CurrentCursor()
	if p.projection != nil && p.projectionCursor == cursor {
		return p.projection, nil
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return nil, err
	}
	projection, err := projectRecordsRaw(records)
	if err != nil {
		return nil, err
	}
	p.projection = projection
	p.projectionCursor = cursor
	return projection, nil
}

// persistHistoryMessages incrementally appends the typed AssistantCommitted
// facts that represent the current accepted history. It is prefix-aware, so
// repeated history observations do not duplicate facts.
func (p *sessionPersistence) persistHistoryMessages(history []messages.Message, turnID string, stepIDs ...string) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	// Runtime history may have been stamped with a local identity before the
	// durable InputQueued fact was reconciled. Match by message value at the
	// same occurrence and carry the durable identity forward; this preserves
	// distinct duplicate texts while making replay/UI IDs converge.
	for i := 0; i < len(projection.History) && i < len(history); i++ {
		if messageIdentityEquivalent(projection.History[i], history[i]) && projection.History[i].ID != "" {
			history[i].ID = projection.History[i].ID
		}
	}
	common := commonMessagePrefix(projection.History, history)
	if common == len(history) && len(projection.History) == len(history) {
		return nil
	}
	if common == len(history) && len(projection.History) > len(history) && len(stepIDs) > 0 {
		// The canonical step path may append tool results after its aggregate
		// ToolResultsProjected fact has already made all results visible in the
		// durable projection.  The live history is intentionally advanced one
		// call at a time, so the candidate is a prefix of the durable history;
		// do not mistake that normal lag for a user rewind/reset.
		return nil
	}
	// A shrink or an edit is represented as a new projection boundary; facts
	// already in the log remain intact for audit/replay.
	var events []session.Event
	if common != len(projection.History) {
		baseCursor := p.store.CurrentCursor()
		txID := stableID("history", struct {
			Cursor  session.Cursor
			TurnID  string
			History []messages.Message
		}{Cursor: baseCursor, TurnID: turnID, History: history})
		resetID := stableID("history-reset", struct {
			Cursor session.Cursor
			TxID   string
		}{Cursor: baseCursor, TxID: txID})
		events = append(events, session.ConversationReset{ResetID: resetID, Reason: "agent history projection replaced"})
		common = 0
	}
	if events == nil {
		events = make([]session.Event, 0, len(history)-common)
	}
	stepID := ""
	if len(stepIDs) > 0 {
		stepID = stepIDs[0]
	}
	for _, message := range history[common:] {
		converted, err := p.messageToSessionLocked(message)
		if err != nil {
			return err
		}
		events = append(events, session.AssistantCommitted{TurnID: turnIDOrLegacy(turnID), StepID: stepID, Message: converted, ToolCalls: sessionMessageToolCalls(converted)})
	}
	if len(events) == 0 {
		return nil
	}
	baseCursor := p.store.CurrentCursor()
	txID := stableID("history", struct {
		Cursor  session.Cursor
		TurnID  string
		History []messages.Message
	}{Cursor: baseCursor, TurnID: turnID, History: history})
	_, err = p.commitLocked(session.Batch{TransactionID: txID, Events: events})
	return err
}

func turnIDOrLegacy(turnID string) string {
	if strings.TrimSpace(turnID) == "" {
		return "turn-legacy"
	}
	return turnID
}

func commonMessagePrefix(left, right []messages.Message) int {
	n := len(left)
	if len(right) < n {
		n = len(right)
	}
	for i := 0; i < n; i++ {
		if !messagesEquivalent(left[i], right[i]) {
			return i
		}
	}
	return n
}

func messagesEquivalent(left, right messages.Message) bool {
	if (left.Role == messages.RoleUser && right.Role == messages.RoleUser) ||
		(left.Role == messages.RoleTool && right.Role == messages.RoleTool) {
		// InputQueued records acceptance time, while the legacy loop stamps the
		// same user message again when runTurn appends it.  The durable queue
		// timestamp is the canonical one; do not rewrite a whole history merely
		// because those two admission timestamps differ.  ToolResultsProjected
		// likewise has a durable, deterministic ID and no runtime timestamp;
		// the live message is initially stamped by NewToolResult.  Both are the
		// same model-visible result when call identity and content agree.
		left.ID = ""
		right.ID = ""
		left.CreatedAt = time.Time{}
		right.CreatedAt = time.Time{}
	}
	return messageFingerprint(left) == messageFingerprint(right)
}

func messageIdentityEquivalent(left, right messages.Message) bool {
	left.ID = ""
	right.ID = ""
	if (left.Role == messages.RoleUser && right.Role == messages.RoleUser) ||
		(left.Role == messages.RoleTool && right.Role == messages.RoleTool) {
		left.CreatedAt = time.Time{}
		right.CreatedAt = time.Time{}
	}
	return messageFingerprint(left) == messageFingerprint(right)
}

func stableID(prefix string, value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(sum[:12])
}

func messageFingerprint(message messages.Message) string {
	data, _ := json.Marshal(message)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// messageToSessionLocked materializes frozen image bytes into the scoped blob
// store before the enclosing AssistantCommitted/InputDelivered projection is
// committed. The caller must hold p.mu.
func (p *sessionPersistence) messageToSessionLocked(message messages.Message) (session.Message, error) {
	for i := range message.ImageAttachments {
		attachment := &message.ImageAttachments[i]
		if attachment.BlobHash != "" {
			if err := validateRequestBlobRef(session.BlobRef{Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType}); err != nil {
				return session.Message{}, err
			}
			continue
		}
		ref, err := p.putBlobLocked(context.Background(), attachment.Data, attachment.MediaType)
		if err != nil {
			return session.Message{}, fmt.Errorf("store image attachment %q: %w", attachment.Path, err)
		}
		if ref == nil {
			return session.Message{}, errors.New("agent: image attachment was not stored")
		}
		attachment.BlobHash, attachment.BlobSize, attachment.MediaType = ref.Hash, ref.Size, ref.MediaType
	}
	return messageToSession(message)
}

// messageToSession converts the current in-memory message into the explicit
// session content union.  No provider-specific or untyped event payload is
// persisted.
func messageToSession(message messages.Message) (session.Message, error) {
	ensureMessageID(&message)
	result := session.Message{ReasoningContent: message.ReasoningContent, MessageID: message.ID, Role: string(message.Role), CreatedAt: message.CreatedAt}
	if result.Role == "" {
		result.Role = string(messages.RoleAssistant)
	}
	if message.Content != "" || len(message.ToolCalls) == 0 {
		result.Content = append(result.Content, session.ContentBlock{Kind: session.ContentText, Text: message.Content})
	}
	for _, attachment := range message.ImageAttachments {
		if attachment.BlobHash == "" {
			// The durable adapter fills this branch before calling the pure
			// converter. Legacy import has no attachment bytes to persist and
			// therefore leaves the original markdown text intact.
			continue
		}
		result.Content = append(result.Content, session.ContentBlock{Kind: session.ContentImage, Path: attachment.Path, Blob: &session.BlobRef{
			Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType,
		}})
	}
	for _, call := range message.ToolCalls {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return session.Message{}, fmt.Errorf("encode tool call %s: %w", call.ID, err)
		}
		result.Content = append(result.Content, session.ContentBlock{Kind: session.ContentToolCall, ToolCall: &session.ToolCall{
			CallID: call.ID, ToolID: call.Name, Arguments: args,
		}})
	}
	if message.Role == messages.RoleTool {
		// A tool result is represented as a typed result block, not free-form
		// assistant text.  The legacy message model carries only an error prefix.
		status := "success"
		text := message.Content
		if strings.HasPrefix(text, "Error: ") {
			status = "error"
			text = strings.TrimPrefix(text, "Error: ")
		}
		result.Content = []session.ContentBlock{{Kind: session.ContentToolResult, ToolResult: &session.ToolResult{
			CallID: message.ToolCallID, Status: status, Text: text, CreatedAt: message.CreatedAt,
		}}}
	}
	if len(result.Content) == 0 {
		result.Content = []session.ContentBlock{{Kind: session.ContentText}}
	}
	return result, nil
}

func messageFromSession(message session.Message) (messages.Message, error) {
	result := messages.Message{ReasoningContent: message.ReasoningContent, ID: message.MessageID, Role: messages.Role(message.Role), CreatedAt: message.CreatedAt}
	if result.Role == "" {
		return messages.Message{}, errors.New("agent: replayed message has no role")
	}
	var text strings.Builder
	for _, block := range message.Content {
		switch block.Kind {
		case session.ContentText:
			text.WriteString(block.Text)
		case session.ContentToolCall:
			if block.ToolCall == nil {
				return messages.Message{}, errors.New("agent: replayed tool call is missing payload")
			}
			var args map[string]any
			decoder := json.NewDecoder(strings.NewReader(string(block.ToolCall.Arguments)))
			decoder.UseNumber()
			if err := decoder.Decode(&args); err != nil || args == nil {
				if err == nil {
					err = errors.New("arguments must be a JSON object")
				}
				return messages.Message{}, fmt.Errorf("agent: replay tool call args: %w", err)
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				if err == nil {
					return messages.Message{}, errors.New("agent: replay tool call args contain trailing JSON")
				}
				return messages.Message{}, fmt.Errorf("agent: replay tool call args: %w", err)
			}
			result.ToolCalls = append(result.ToolCalls, messages.ToolCall{ID: block.ToolCall.CallID, Name: block.ToolCall.ToolID, Arguments: args})
		case session.ContentToolResult:
			if block.ToolResult == nil {
				return messages.Message{}, errors.New("agent: replayed tool result is missing payload")
			}
			result.ToolCallID = block.ToolResult.CallID
			result.Content = block.ToolResult.Text
			if block.ToolResult.Status != "success" && block.ToolResult.Status != "ok" && !strings.HasPrefix(result.Content, "Error: ") {
				result.Content = "Error: " + result.Content
			}
			result.CreatedAt = block.ToolResult.CreatedAt
		case session.ContentImage:
			if block.Blob == nil {
				return messages.Message{}, errors.New("agent: replayed image is missing blob")
			}
			result.ImagesFrozen = true
			result.ImageAttachments = append(result.ImageAttachments, messages.ImageAttachment{Path: block.Path, MediaType: block.Blob.MediaType, BlobHash: block.Blob.Hash, BlobSize: block.Blob.Size})
		default:
			return messages.Message{}, fmt.Errorf("agent: replayed message has unknown content kind %q", block.Kind)
		}
	}
	if result.Content == "" && text.Len() > 0 {
		result.Content = text.String()
	}
	return result, nil
}

// legacyEvents translates only data present in the old snapshot.  It does
// not infer requests, approvals, tool starts, or successful side effects.
func legacyEvents(snapshot SessionSnapshot, sourcePath string) ([]session.Event, error) {
	createdAt := snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = snapshot.UpdatedAt
	}
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 0).UTC()
	}
	importedAt := snapshot.UpdatedAt
	if importedAt.IsZero() {
		importedAt = createdAt
	}
	source := legacyImportSource
	if sourcePath == "" {
		source = "resume-snapshot"
	}
	importMarker := session.SessionImported{
		SessionID: snapshot.ID, FormatVersion: session.SchemaVersion, Source: source,
		OriginalPath: sourcePath, ImportedAt: importedAt,
		ParentID: snapshot.ParentID, BranchPoint: snapshot.BranchPoint, BranchSummary: snapshot.BranchSummary,
	}
	// Keep the import marker out of the data chunks. Readers treat the marker's
	// dedicated final transaction as the commit point; a crash after any
	// earlier chunk therefore leaves the target non-authoritative and lets the
	// explicit Resume path retry the deterministic chunks.
	events := make([]session.Event, 0, len(snapshot.History)*2+3)
	if snapshot.Model != "" || snapshot.Workspace != "" {
		events = append(events, session.SettingsChanged{Revision: 1, Settings: session.Settings{Model: snapshot.Model, Workspace: snapshot.Workspace}})
	}
	if snapshot.Usage != (Usage{}) {
		events = append(events, session.UsageChanged{Revision: 1, Usage: session.Usage{
			InputTokens: int64(snapshot.Usage.InputTokens), OutputTokens: int64(snapshot.Usage.OutputTokens),
			CachedTokens: int64(snapshot.Usage.CachedTokens), TotalTokens: int64(snapshot.Usage.InputTokens + snapshot.Usage.OutputTokens),
			Cost: snapshot.Usage.Cost, TurnCount: int64(snapshot.Usage.TurnCount),
		}})
	}
	if snapshot.Settings != nil {
		settings := *snapshot.Settings
		events = append(events, session.SettingsChanged{Revision: 1, Settings: settings})
	}
	if snapshot.Workflow != nil {
		events = append(events, session.WorkflowChanged{Workflow: *snapshot.Workflow})
	}
	if snapshot.Memory != "" {
		events = append(events, session.MemoryChanged{Revision: 1, Text: snapshot.Memory})
	}
	if len(snapshot.Tasks) > 0 {
		events = append(events, session.TasksChanged{Revision: 1, Tasks: append([]session.Task(nil), snapshot.Tasks...)})
	}
	for i, message := range snapshot.History {
		turnID := fmt.Sprintf("legacy-turn-%06d", i)
		message.ID = legacyMessageID(snapshot.ID, i)
		if message.Role == messages.RoleUser && strings.TrimSpace(message.Content) != "" {
			inputID := fmt.Sprintf("legacy-input-%06d", i)
			events = append(events,
				session.InputQueued{InputID: inputID, MessageID: message.ID, Text: message.Content, Strategy: "followup", TurnID: turnID, CreatedAt: message.CreatedAt},
				session.InputDelivered{InputID: inputID, TurnID: turnID},
			)
			continue
		}
		converted, err := messageToSession(message)
		if err != nil {
			return nil, fmt.Errorf("legacy history[%d]: %w", i, err)
		}
		events = append(events, session.AssistantCommitted{TurnID: turnID, Message: converted, ToolCalls: sessionMessageToolCalls(converted)})
	}
	pendingInputs := pendingInputSnapshot(snapshot.ID, snapshot.Pending, snapshot.PendingInputs)
	for i, text := range snapshot.Pending {
		if strings.TrimSpace(text) == "" {
			continue
		}
		inputID := fmt.Sprintf("legacy-pending-%06d", i)
		strategy := protocol.InputFollowup
		if i < len(pendingInputs) {
			if pendingInputs[i].ID != "" {
				inputID = string(pendingInputs[i].ID)
			}
			if pendingInputs[i].Strategy == protocol.InputSteer || pendingInputs[i].Strategy == protocol.InputFollowup {
				strategy = pendingInputs[i].Strategy
			}
		}
		events = append(events, session.InputQueued{InputID: inputID, MessageID: inputMessageID(inputID), Text: text, Strategy: string(strategy), CreatedAt: importedAt})
	}
	events = append(events, importMarker)
	return events, nil
}

func sessionMessageToolCalls(message session.Message) []session.ToolCall {
	var calls []session.ToolCall
	for _, block := range message.Content {
		if block.Kind == session.ContentToolCall && block.ToolCall != nil {
			calls = append(calls, *block.ToolCall)
		}
	}
	return calls
}

// importLegacySnapshot is called only from explicit Resume paths.  It keeps
// the old file untouched and uses deterministic transaction/event IDs so a
// crash during a multi-batch import can be resumed without duplicates.
func importLegacySnapshot(cfg *config.Config, snapshot SessionSnapshot, sourcePath string) (*sessionPersistence, error) {
	p, err := openSessionPersistenceSource(cfg, snapshot.ID, snapshot.CreatedAt, "legacy-import")
	if err != nil {
		return nil, err
	}
	events, err := legacyEvents(snapshot, sourcePath)
	if err != nil {
		_ = p.close()
		return nil, err
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		_ = p.close()
		return nil, err
	}
	for _, record := range records {
		if record.TransactionID == legacyImportComplete && record.Event.Type() == session.EventTypeSessionImported {
			return p, nil
		}
	}
	if len(events) == 0 {
		_ = p.close()
		return nil, errors.New("agent: legacy import has no completion marker")
	}
	// The marker is always the final event and gets its own transaction. Keep
	// each data transaction comfortably below the Store event bound while
	// retaining deterministic chunk identity for retries.
	marker := events[len(events)-1]
	dataEvents := events[:len(events)-1]
	for offset, chunk := 0, 0; offset < len(dataEvents); chunk++ {
		end := offset + 128
		if end > len(dataEvents) {
			end = len(dataEvents)
		}
		txID := legacyImportBatch
		if chunk > 0 {
			txID = fmt.Sprintf("%s-%04d", legacyImportBatch, chunk)
		}
		if _, err := p.Commit(session.Batch{TransactionID: txID, Events: dataEvents[offset:end]}); err != nil {
			_ = p.close()
			return nil, fmt.Errorf("agent: import legacy batch %d: %w", chunk, err)
		}
		offset = end
	}
	if _, err := p.Commit(session.Batch{TransactionID: legacyImportComplete, Events: []session.Event{marker}}); err != nil {
		_ = p.close()
		return nil, fmt.Errorf("agent: import legacy completion: %w", err)
	}
	return p, nil
}

func legacyImportEvidence(records []session.Record) bool {
	for _, record := range records {
		switch event := record.Event.(type) {
		case *session.SessionCreated:
			if event.Source == "legacy-import" {
				return true
			}
		case *session.SessionImported:
			if event.Source == legacyImportSource || event.Source == "resume-snapshot" {
				return true
			}
		}
	}
	return false
}

func legacyImportCompletePresent(records []session.Record) bool {
	for _, record := range records {
		if record.TransactionID == legacyImportComplete && record.Event.Type() == session.EventTypeSessionImported {
			return true
		}
	}
	return false
}

// SessionSnapshotProjection replays a read-only store into the compatibility
// snapshot shape used by current export/UI code.  It never consults or writes
// snapshot.json; callers can use it for deterministic replay tests.
func SessionSnapshotProjection(store session.ReadOnlyStore, id string) (*SessionSnapshot, error) {
	if store == nil {
		return nil, errors.New("agent: nil session store")
	}
	records, err := session.ReadAll(store)
	if err != nil {
		return nil, err
	}
	return snapshotFromRecords(records, id)
}

// snapshotFromOwnedStore rereads the authoritative log while this Agent's
// writer is held. Resume may have loaded a snapshot before acquiring that
// lock; callers should use this method after opening the writer so a concurrent
// commit cannot leave the new Agent initialized from stale projection data.
func (p *sessionPersistence) snapshotFromOwnedStore(id string) (*SessionSnapshot, error) {
	if p == nil {
		return nil, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return nil, session.ErrClosed
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return nil, err
	}
	return snapshotFromRecords(records, id)
}

// populateMemoryResume seeds the per-process MemoryStore used by
// --no-session-persistence Resume. There is no disk log to replay in that
// mode, so the caller's explicit snapshot must be converted into the same
// typed facts that a JSONL replay would expose before the owned-store replay
// boundary runs. It is intentionally a no-op for JSONL stores.
func (p *sessionPersistence) populateMemoryResume(snapshot *SessionSnapshot) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if snapshot == nil {
		return errors.New("agent: memory resume snapshot is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	if _, ok := p.store.(*session.MemoryStore); !ok {
		return nil
	}
	if p.failed != nil {
		return fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if snapshot.ID == "" {
		return errors.New("agent: memory resume snapshot has no session id")
	}
	if err := validateSessionID(snapshot.ID); err != nil {
		return err
	}
	if p.sessionID != "" && p.sessionID != snapshot.ID {
		return fmt.Errorf("agent: memory resume session %q does not match persistence %q", snapshot.ID, p.sessionID)
	}

	records, err := session.ReadAll(p.store)
	if err != nil {
		return err
	}
	for _, record := range records {
		if imported, ok := record.Event.(*session.SessionImported); ok && imported.Source == "memory-resume" {
			// The helper is idempotent even when a caller retries construction
			// after an uncertain in-memory commit.
			return nil
		}
	}
	events, err := memoryResumeEvents(*snapshot)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return errors.New("agent: memory resume has no events")
	}
	baseID := stableID("memory-resume", struct {
		ID            string
		CreatedAt     time.Time
		UpdatedAt     time.Time
		History       []messages.Message
		Usage         Usage
		Pending       []string
		PendingInputs []protocol.InputView
		ParentID      string
		BranchPoint   int
		BranchSummary string
	}{snapshot.ID, snapshot.CreatedAt, snapshot.UpdatedAt, snapshot.History, snapshot.Usage, snapshot.Pending, snapshot.PendingInputs, snapshot.ParentID, snapshot.BranchPoint, snapshot.BranchSummary})
	for offset, chunk := 0, 0; offset < len(events); chunk++ {
		end := offset + 128
		if end > len(events) {
			end = len(events)
		}
		if _, err := p.commitLocked(session.Batch{
			TransactionID: fmt.Sprintf("%s-%04d", baseID, chunk),
			Events:        events[offset:end],
		}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

// memoryResumeEvents maps a caller-owned snapshot to typed facts while
// retaining existing message IDs and timestamps. Unlike legacy import, this
// path is not an import boundary and therefore uses a distinct SessionImported
// source without touching a legacy file or requiring an import-complete marker.
func memoryResumeEvents(snapshot SessionSnapshot) ([]session.Event, error) {
	createdAt := snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = snapshot.UpdatedAt
	}
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 0).UTC()
	}
	importedAt := snapshot.UpdatedAt
	if importedAt.IsZero() {
		importedAt = createdAt
	}
	events := make([]session.Event, 0, len(snapshot.History)*2+len(snapshot.Pending)+3)
	if snapshot.Workspace != "" || snapshot.Model != "" {
		events = append(events, session.SettingsChanged{Revision: 1, Settings: session.Settings{Workspace: snapshot.Workspace, Model: snapshot.Model}})
	}
	if snapshot.Usage != (Usage{}) {
		events = append(events, session.UsageChanged{Revision: 1, Usage: session.Usage{
			InputTokens: int64(snapshot.Usage.InputTokens), OutputTokens: int64(snapshot.Usage.OutputTokens),
			CachedTokens: int64(snapshot.Usage.CachedTokens), TotalTokens: int64(snapshot.Usage.InputTokens + snapshot.Usage.OutputTokens),
			Cost: snapshot.Usage.Cost, TurnCount: int64(snapshot.Usage.TurnCount),
		}})
	}
	if snapshot.Settings != nil {
		events = append(events, session.SettingsChanged{Revision: 1, Settings: *snapshot.Settings})
	}
	if snapshot.PendingSettings != nil {
		candidate := *snapshot.PendingSettings
		events = append(events, session.SettingsScheduled{ChangeID: candidate.ChangeID, Settings: candidate.Settings, SourceRevision: candidate.SourceRevision})
	}
	for _, command := range snapshot.PendingCommands {
		// The command payload is intentionally absent from the snapshot.  A
		// resumed no-session runtime can therefore expose the admitted operation
		// as unknown, but can never reconstruct and re-run its side effect.
		events = append(events, command)
	}
	if snapshot.Workflow != nil {
		events = append(events, session.WorkflowChanged{Workflow: *snapshot.Workflow})
	}
	if snapshot.Memory != "" {
		events = append(events, session.MemoryChanged{Revision: 1, Text: snapshot.Memory})
	}
	if len(snapshot.Tasks) > 0 {
		events = append(events, session.TasksChanged{Revision: 1, Tasks: append([]session.Task(nil), snapshot.Tasks...)})
	}
	for i, original := range snapshot.History {
		message := original
		if message.ID == "" {
			message.ID = legacyMessageID(snapshot.ID, i)
		}
		turnID := fmt.Sprintf("memory-resume-turn-%06d", i)
		if message.Role == messages.RoleUser && strings.TrimSpace(message.Content) != "" {
			inputID := fmt.Sprintf("memory-resume-input-%06d", i)
			converted, err := messageToSession(message)
			if err != nil {
				return nil, fmt.Errorf("memory resume history[%d]: %w", i, err)
			}
			// The InputQueued MessageID is the authoritative history identity;
			// messageToSession also validates the role/content conversion.
			events = append(events,
				session.InputQueued{InputID: inputID, MessageID: converted.MessageID, Text: message.Content, Strategy: "followup", TurnID: turnID, CreatedAt: message.CreatedAt},
				session.InputDelivered{InputID: inputID, TurnID: turnID},
			)
			continue
		}
		converted, err := messageToSession(message)
		if err != nil {
			return nil, fmt.Errorf("memory resume history[%d]: %w", i, err)
		}
		events = append(events, session.AssistantCommitted{TurnID: turnID, Message: converted, ToolCalls: sessionMessageToolCalls(converted)})
	}
	for _, input := range pendingInputSnapshot(snapshot.ID, snapshot.Pending, snapshot.PendingInputs) {
		if strings.TrimSpace(input.Text) == "" || input.ID == "" {
			continue
		}
		strategy := input.Strategy
		if strategy != protocol.InputSteer && strategy != protocol.InputFollowup {
			strategy = protocol.InputFollowup
		}
		created := input.CreatedAt
		if created.IsZero() {
			created = importedAt
		}
		events = append(events, session.InputQueued{
			InputID: string(input.ID), MessageID: inputMessageID(string(input.ID)), Text: input.Text,
			Strategy: string(strategy), CreatedAt: created,
		})
	}
	events = append(events, session.SessionImported{
		SessionID: snapshot.ID, FormatVersion: session.SchemaVersion, Source: "memory-resume", ImportedAt: importedAt,
		ParentID: snapshot.ParentID, BranchPoint: snapshot.BranchPoint, BranchSummary: snapshot.BranchSummary,
	})
	return events, nil
}

func snapshotFromRecords(records []session.Record, id string) (*SessionSnapshot, error) {
	if legacyImportEvidence(records) && !legacyImportCompletePresent(records) {
		return nil, fmt.Errorf("agent: session has an incomplete legacy import")
	}
	projection, err := projectRecords(records)
	if err != nil {
		return nil, err
	}
	if projection.ID == "" {
		projection.ID = id
	}
	if id != "" && projection.ID != id {
		return nil, fmt.Errorf("agent: replayed session id %q does not match requested id %q", projection.ID, id)
	}
	return snapshotFromProjection(projection, id), nil
}

func snapshotFromProjection(projection *replayProjection, id string) *SessionSnapshot {
	if projection == nil {
		return nil
	}
	if projection.ID == "" {
		projection.ID = id
	}
	return &SessionSnapshot{
		ID: projection.ID, CreatedAt: projection.CreatedAt, UpdatedAt: projection.updatedAt,
		Workspace: projection.Workspace, Model: projection.Model, Title: sessionTitle(projection.History),
		History: cloneMessages(projection.History), Usage: projection.Usage, Pending: append([]string(nil), projection.Pending...), PendingInputs: projection.pendingInputViews(), PendingAttachments: projection.pendingAttachments(),
		Settings: snapshotSettings(projection.Settings), PendingSettings: snapshotPendingSettings(projection.PendingSettings), Workflow: snapshotWorkflow(projection.Workflow),
		Memory: projection.MemoryText, Tasks: append([]session.Task(nil), projection.Tasks...),
		PendingCommands: projection.pendingCommands(),
		ParentID:        projection.ParentID, BranchPoint: projection.BranchPoint, BranchSummary: projection.BranchSummary,
		turnSeq: projection.turnSeq, stepSeq: projection.stepSeq,
	}
}

func snapshotSettings(settings session.Settings) *session.Settings {
	if reflect.DeepEqual(settings, session.Settings{}) {
		return nil
	}
	copy := settings
	if settings.Temperature != nil {
		value := *settings.Temperature
		copy.Temperature = &value
	}
	if settings.MaxOutputTokens != nil {
		value := *settings.MaxOutputTokens
		copy.MaxOutputTokens = &value
	}
	copy.AlwaysAllow = append([]string(nil), settings.AlwaysAllow...)
	copy.AlwaysDeny = append([]string(nil), settings.AlwaysDeny...)
	copy.AdditionalDirectories = append([]string(nil), settings.AdditionalDirectories...)
	copy.DisallowedDirectories = append([]string(nil), settings.DisallowedDirectories...)
	return &copy
}

func snapshotPendingSettings(settings *session.SettingsScheduled) *session.SettingsScheduled {
	if settings == nil {
		return nil
	}
	copy := *settings
	if snapshot := snapshotSettings(settings.Settings); snapshot != nil {
		copy.Settings = *snapshot
	}
	return &copy
}

func snapshotWorkflow(workflow session.WorkflowState) *session.WorkflowState {
	if workflow == (session.WorkflowState{}) {
		return nil
	}
	copy := workflow
	return &copy
}

type replayProjection struct {
	ID        string
	CreatedAt time.Time
	updatedAt time.Time
	// turnSeq/stepSeq are monotonic counters derived from durable fact IDs.
	// They are not a projection of usage or history; they only prevent a
	// resumed Agent from reusing a transaction identity already in the log.
	turnSeq           uint64
	stepSeq           uint64
	Workspace         string
	Model             string
	Settings          session.Settings
	SettingsRev       uint64
	PendingSettings   *session.SettingsScheduled
	ScheduledCommands map[string]session.CommandScheduled
	CompletedCommands map[string]session.CommandCompleted
	Workflow          session.WorkflowState
	MemoryText        string
	memorySeen        bool
	Tasks             []session.Task
	CatalogVersion    uint64
	History           []messages.Message
	historyStep       []string // parallel step identity for typed assistant/tool facts
	Usage             Usage
	Pending           []string
	pendingOrder      []string
	pendingByID       map[string]messages.Message
	pendingInput      map[string]session.InputQueued
	startedTools      map[toolFactKey]session.ToolStarted
	startedOrder      []toolFactKey
	finishedTools     map[toolFactKey]session.ToolFinished
	finishedOrder     []toolFactKey
	projected         map[toolFactKey]bool
	ParentID          string
	BranchPoint       int
	BranchSummary     string
	Discovered        map[string]session.ToolSchema
}

func runtimeSequence(id, prefix string) uint64 {
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(id, prefix), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (p *replayProjection) observeRuntimeSequence(turnID, stepID string) {
	if p == nil {
		return
	}
	if n := runtimeSequence(turnID, "turn-"); n > p.turnSeq {
		p.turnSeq = n
	}
	if n := runtimeSequence(stepID, "step-"); n > p.stepSeq {
		p.stepSeq = n
	}
}

type toolFactKey struct {
	TurnID string
	StepID string
	CallID string
}

func makeToolFactKey(turnID, stepID, callID string) toolFactKey {
	return toolFactKey{TurnID: turnID, StepID: stepID, CallID: callID}
}

func (p *replayProjection) resolveToolFactKey(turnID, stepID, callID string) toolFactKey {
	key := makeToolFactKey(turnID, stepID, callID)
	if stepID != "" {
		return key
	}
	// Old facts did not carry StepID. If exactly one matching step exists,
	// attach the completion/projection to it; otherwise retain the empty-step
	// key so ambiguous legacy data is never merged across calls.
	var match toolFactKey
	found := false
	for candidate := range p.startedTools {
		if candidate.TurnID != turnID || candidate.CallID != callID {
			continue
		}
		if found {
			return key
		}
		match, found = candidate, true
	}
	if found {
		return match
	}
	for candidate := range p.finishedTools {
		if candidate.TurnID != turnID || candidate.CallID != callID {
			continue
		}
		if found && candidate != match {
			return key
		}
		match, found = candidate, true
	}
	if found {
		return match
	}
	return key
}

func (p *replayProjection) pendingInputViews() []protocol.InputView {
	if p == nil || len(p.pendingOrder) == 0 {
		return nil
	}
	views := make([]protocol.InputView, 0, len(p.pendingOrder))
	for _, inputID := range p.pendingOrder {
		input, ok := p.pendingInput[inputID]
		if !ok || input.Text == "" {
			continue
		}
		strategy := protocol.InputStrategy(input.Strategy)
		if strategy != protocol.InputSteer && strategy != protocol.InputFollowup {
			strategy = protocol.InputFollowup
		}
		views = append(views, protocol.InputView{
			ID: protocol.InputID(input.InputID), Text: input.Text, Strategy: strategy, State: "queued", CreatedAt: input.CreatedAt,
		})
	}
	return views
}

func (p *replayProjection) pendingAttachments() map[string][]messages.ImageAttachment {
	if p == nil || len(p.pendingByID) == 0 {
		return nil
	}
	out := make(map[string][]messages.ImageAttachment)
	for inputID, message := range p.pendingByID {
		if len(message.ImageAttachments) == 0 {
			continue
		}
		out[inputID] = cloneImageAttachments(message.ImageAttachments)
	}
	return out
}

func newReplayProjection() *replayProjection {
	return &replayProjection{
		pendingByID: make(map[string]messages.Message), pendingInput: make(map[string]session.InputQueued),
		startedTools: make(map[toolFactKey]session.ToolStarted), finishedTools: make(map[toolFactKey]session.ToolFinished), projected: make(map[toolFactKey]bool),
		ScheduledCommands: make(map[string]session.CommandScheduled), CompletedCommands: make(map[string]session.CommandCompleted),
		Discovered: make(map[string]session.ToolSchema),
	}
}

func (p *replayProjection) pendingCommands() []session.CommandScheduled {
	if p == nil || len(p.ScheduledCommands) == 0 {
		return nil
	}
	ids := make([]string, 0, len(p.ScheduledCommands))
	for id := range p.ScheduledCommands {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]session.CommandScheduled, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.ScheduledCommands[id])
	}
	return out
}

func projectRecords(records []session.Record) (*replayProjection, error) {
	p, err := projectRecordsRaw(records)
	if err != nil {
		return nil, err
	}
	p.finalizeRecovery()
	return p, nil
}

// projectRecordsRaw replays only durable facts. Synthetic recovery results are
// deliberately added by finalizeRecovery on a read projection, never cached
// in sessionPersistence: a subsequent raw completion must be able to close a
// Started call without duplicating an "unknown" message.
func projectRecordsRaw(records []session.Record) (*replayProjection, error) {
	p := newReplayProjection()
	for _, record := range records {
		if err := p.apply(record.Event); err != nil {
			return nil, fmt.Errorf("agent: replay record %d: %w", record.Seq, err)
		}
	}
	return p, nil
}

func (p *replayProjection) finalizeRecovery() {
	if p == nil {
		return
	}
	// A started tool without a durable completion is deliberately projected as
	// unknown.  It is never automatically re-run by this layer.
	for _, key := range p.orderedStartedIDs() {
		started := p.startedTools[key]
		if _, finished := p.finishedTools[key]; finished {
			continue
		}
		if p.projected[key] {
			// A projection without a raw completion cannot prove that the
			// side effect completed. Replace it with the honest unknown state
			// rather than claiming a successful result.
			p.removeProjectedToolResult(key)
		}
		if started.Call.CallID == "" {
			continue
		}
		unknown := messages.Message{Role: messages.RoleTool, ToolCallID: key.CallID,
			Content: "Error: tool result unknown (the tool may have executed before the session stopped); it will not be re-run automatically"}
		unknown.ID = stableID("unknown-tool-result", key)
		p.insertRecoveredToolResult(key, unknown)
	}
	// If a raw completion was durably admitted but the batch projection was
	// lost during a crash, recover a legal tool-result pair from the raw facts
	// without executing the tool again. Sort by the frozen call ordinal when
	// available; old logs retain their append order via rawFinishedOrder.
	for _, key := range p.orderedFinishedIDs() {
		if p.projected[key] {
			continue
		}
		finished, ok := p.finishedTools[key]
		if !ok {
			continue
		}
		result := finished.Result
		content := result.Text
		if result.Status != "success" && result.Status != "ok" && !strings.HasPrefix(content, "Error: ") {
			content = "Error: " + content
		}
		message := messages.Message{Role: messages.RoleTool, ToolCallID: key.CallID, Content: content}
		message.ID = toolResultMessageID(key, result.Text, result.Status)
		p.insertRecoveredToolResult(key, message)
		p.projected[key] = true
	}
}

// orderedStartedIDs and orderedFinishedIDs retain deterministic call order
// even when ToolFinished facts arrive from parallel workers in completion
// order. CallIndex is the new explicit ordinal; insertion order is the
// compatibility fallback for old events that have no ordinal.
func (p *replayProjection) orderedStartedIDs() []toolFactKey {
	if p == nil {
		return nil
	}
	ids := append([]toolFactKey(nil), p.startedOrder...)
	seen := make(map[toolFactKey]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for id := range p.startedTools {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		a, aok := p.startedTools[ids[i]]
		b, bok := p.startedTools[ids[j]]
		if aok && bok && ids[i].TurnID == ids[j].TurnID && ids[i].StepID != "" && ids[i].StepID == ids[j].StepID && a.CallIndex != b.CallIndex {
			return a.CallIndex < b.CallIndex
		}
		return false
	})
	return ids
}

func (p *replayProjection) orderedFinishedIDs() []toolFactKey {
	if p == nil {
		return nil
	}
	ids := append([]toolFactKey(nil), p.finishedOrder...)
	seen := make(map[toolFactKey]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for id := range p.finishedTools {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	// Stable sort by the corresponding Started ordinal only within a step;
	// equal/legacy ordinals retain the raw completion order.
	sort.SliceStable(ids, func(i, j int) bool {
		a, aok := p.startedTools[ids[i]]
		b, bok := p.startedTools[ids[j]]
		if aok && bok && ids[i].TurnID == ids[j].TurnID && ids[i].StepID != "" && ids[i].StepID == ids[j].StepID && a.CallIndex != b.CallIndex {
			return a.CallIndex < b.CallIndex
		}
		return false
	})
	return ids
}

func (p *replayProjection) removeProjectedToolResult(key toolFactKey) {
	if p == nil || key.CallID == "" {
		return
	}
	filtered := p.History[:0]
	filteredSteps := p.historyStep[:0]
	for i, message := range p.History {
		stepID := ""
		if i < len(p.historyStep) {
			stepID = p.historyStep[i]
		}
		if message.Role == messages.RoleTool && message.ToolCallID == key.CallID && (key.StepID == "" || stepID == key.StepID) {
			continue
		}
		filtered = append(filtered, message)
		filteredSteps = append(filteredSteps, stepID)
	}
	p.History = filtered
	p.historyStep = filteredSteps
	delete(p.projected, key)
}

func (p *replayProjection) insertRecoveredToolResult(key toolFactKey, recovered messages.Message) {
	if p == nil || recovered.ToolCallID == "" {
		return
	}
	for i, assistant := range p.History {
		if assistant.Role != messages.RoleAssistant || !assistantHasCall(assistant, key.CallID) {
			continue
		}
		if key.StepID != "" && (i >= len(p.historyStep) || p.historyStep[i] != key.StepID) {
			continue
		}
		end := i + 1
		existing := make(map[string]messages.Message)
		existingSteps := make(map[string]string)
		for end < len(p.History) && p.History[end].Role == messages.RoleTool {
			result := p.History[end]
			if _, seen := existing[result.ToolCallID]; !seen {
				existing[result.ToolCallID] = result
				if end < len(p.historyStep) {
					existingSteps[result.ToolCallID] = p.historyStep[end]
				}
			}
			end++
		}
		if _, already := existing[key.CallID]; already {
			// A repeated call ID in a later step must be associated with the
			// next assistant block, not the already-complete earlier block.
			continue
		}
		ordered := make([]messages.Message, 0, len(assistant.ToolCalls))
		orderedSteps := make([]string, 0, len(assistant.ToolCalls))
		for _, call := range assistant.ToolCalls {
			if result, ok := existing[call.ID]; ok {
				ordered = append(ordered, result)
				orderedSteps = append(orderedSteps, existingSteps[call.ID])
			} else if call.ID == key.CallID {
				ordered = append(ordered, recovered)
				orderedSteps = append(orderedSteps, key.StepID)
			}
		}
		for offset, result := range p.History[i+1 : end] {
			if !assistantHasCall(assistant, result.ToolCallID) {
				ordered = append(ordered, result)
				stepID := ""
				if i+1+offset < len(p.historyStep) {
					stepID = p.historyStep[i+1+offset]
				}
				orderedSteps = append(orderedSteps, stepID)
			}
		}
		updated := make([]messages.Message, 0, len(p.History)+1)
		updatedSteps := make([]string, 0, len(p.History)+1)
		updated = append(updated, p.History[:i+1]...)
		if i+1 <= len(p.historyStep) {
			updatedSteps = append(updatedSteps, p.historyStep[:i+1]...)
		} else {
			for n := 0; n < i+1; n++ {
				updatedSteps = append(updatedSteps, "")
			}
		}
		updated = append(updated, ordered...)
		updatedSteps = append(updatedSteps, orderedSteps...)
		updated = append(updated, p.History[end:]...)
		if end < len(p.historyStep) {
			updatedSteps = append(updatedSteps, p.historyStep[end:]...)
		} else {
			for n := end; n < len(p.History); n++ {
				updatedSteps = append(updatedSteps, "")
			}
		}
		p.History = updated
		p.historyStep = updatedSteps
		return
	}
	// If the assistant fact is from a legacy projection that omitted the call
	// payload, keep the recovery fact visible rather than silently dropping it.
	p.History = append(p.History, recovered)
	p.historyStep = append(p.historyStep, key.StepID)
}

func assistantHasCall(message messages.Message, callID string) bool {
	for _, call := range message.ToolCalls {
		if call.ID == callID {
			return true
		}
	}
	return false
}

// toolResultMessageID is occurrence-scoped.  A provider is allowed to reuse a
// call ID in a later step, and equal output is not evidence that the two
// messages are the same conversation entry.  Keep the old content-only shape
// for legacy facts which have no occurrence identity; all new projections have
// a turn/step key and therefore cannot collide in the UI or replay cache.
func toolResultMessageID(key toolFactKey, text, status string) string {
	if key.TurnID == "" && key.StepID == "" {
		return stableID("tool-result", struct {
			CallID string
			Text   string
			Status string
		}{CallID: key.CallID, Text: text, Status: status})
	}
	return stableID("tool-result", struct {
		TurnID string
		StepID string
		CallID string
		Text   string
		Status string
	}{TurnID: key.TurnID, StepID: key.StepID, CallID: key.CallID, Text: text, Status: status})
}

func (p *replayProjection) apply(event session.Event) error {
	switch e := event.(type) {
	case *session.SessionCreated:
		p.ID, p.CreatedAt = e.SessionID, e.CreatedAt
		if e.ParentID != "" {
			p.ParentID = e.ParentID
		}
		p.updatedAt = e.CreatedAt
	case *session.SessionImported:
		if p.ID == "" {
			p.ID = e.SessionID
		}
		p.ParentID, p.BranchPoint, p.BranchSummary = e.ParentID, e.BranchPoint, e.BranchSummary
		if e.ImportedAt.After(p.updatedAt) {
			p.updatedAt = e.ImportedAt
		}
	case *session.SettingsChanged:
		// SettingsChanged is a frozen public session snapshot. Keep the full
		// value for resume/replay (credentials never belong in session.Settings)
		// while retaining the legacy Workspace/Model projection fields.
		p.Settings = e.Settings
		p.SettingsRev = e.Revision
		p.Model, p.Workspace = e.Settings.Model, e.Settings.Workspace
		p.PendingSettings = nil
	case *session.SettingsScheduled:
		candidate := *e
		if settings := snapshotSettings(e.Settings); settings != nil {
			candidate.Settings = *settings
		}
		p.PendingSettings = &candidate
	case *session.SettingsScheduleCancelled:
		if p.PendingSettings != nil && p.PendingSettings.ChangeID == e.ChangeID {
			p.PendingSettings = nil
		}
	case *session.CommandScheduled:
		if prior, exists := p.ScheduledCommands[e.CommandID]; exists && prior.InputDigest != e.InputDigest {
			return fmt.Errorf("command %q was scheduled with different input digest", e.CommandID)
		}
		p.ScheduledCommands[e.CommandID] = *e
	case *session.CommandCompleted:
		p.CompletedCommands[e.CommandID] = *e
		delete(p.ScheduledCommands, e.CommandID)
	case *session.InputQueued:
		p.observeRuntimeSequence(e.TurnID, "")
		if _, exists := p.pendingInput[e.InputID]; !exists {
			p.pendingOrder = append(p.pendingOrder, e.InputID)
		}
		p.pendingInput[e.InputID] = *e
		if e.Text != "" {
			pending := messages.Message{Role: messages.RoleUser, Content: e.Text, CreatedAt: e.CreatedAt}
			pending.ID = e.MessageID
			if pending.ID == "" {
				pending.ID = inputMessageID(e.InputID)
			}
			pending.ImagesFrozen = e.ImagesFrozen
			for _, ref := range e.Attachments {
				pending.ImageAttachments = append(pending.ImageAttachments, messages.ImageAttachment{MediaType: ref.MediaType, BlobHash: ref.Hash, BlobSize: ref.Size})
			}
			p.pendingByID[e.InputID] = pending
		}
	case *session.InputDelivered:
		p.observeRuntimeSequence(e.TurnID, "")
		if message, ok := p.pendingByID[e.InputID]; ok {
			p.History = append(p.History, message)
			p.historyStep = append(p.historyStep, "")
			delete(p.pendingByID, e.InputID)
		}
		delete(p.pendingInput, e.InputID)
		p.removePendingID(e.InputID)
	case *session.InputCancelled:
		delete(p.pendingByID, e.InputID)
		delete(p.pendingInput, e.InputID)
		p.removePendingID(e.InputID)
	case *session.AssistantCommitted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		message, err := messageFromSession(e.Message)
		if err != nil {
			return err
		}
		p.History = append(p.History, message)
		p.historyStep = append(p.historyStep, e.StepID)
	case *session.ToolStarted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		key := makeToolFactKey(e.TurnID, e.StepID, e.Call.CallID)
		if _, exists := p.startedTools[key]; !exists {
			p.startedOrder = append(p.startedOrder, key)
		}
		p.startedTools[key] = *e
	case *session.ToolFinished:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		key := p.resolveToolFactKey(e.TurnID, e.StepID, e.CallID)
		if _, exists := p.finishedTools[key]; !exists {
			p.finishedOrder = append(p.finishedOrder, key)
		}
		p.finishedTools[key] = *e
	case *session.ToolResultsProjected:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		for _, result := range e.Results {
			key := p.resolveToolFactKey(e.TurnID, e.StepID, result.CallID)
			p.projected[key] = true
			content := result.Text
			if result.Status != "success" && result.Status != "ok" && !strings.HasPrefix(content, "Error: ") {
				content = "Error: " + content
			}
			toolMessage := messages.Message{Role: messages.RoleTool, ToolCallID: result.CallID, Content: content}
			toolMessage.ID = toolResultMessageID(key, result.Text, result.Status)
			p.History = append(p.History, toolMessage)
			p.historyStep = append(p.historyStep, e.StepID)
		}
	case *session.MemoryChanged:
		p.memorySeen = true
		if e.Cleared {
			p.MemoryText = ""
		} else if e.Text != "" {
			p.MemoryText = e.Text
		}
	case *session.TasksChanged:
		p.Tasks = append([]session.Task(nil), e.Tasks...)
	case *session.WorkflowChanged:
		p.Workflow = e.Workflow
	case *session.ContextCompacted:
		message, err := messageFromSession(e.Summary)
		if err != nil {
			return err
		}
		p.History = []messages.Message{message}
		p.historyStep = []string{""}
	case *session.ConversationReset:
		p.History = nil
		p.historyStep = nil
	case *session.UsageChanged:
		p.Usage = Usage{InputTokens: int(e.Usage.InputTokens), OutputTokens: int(e.Usage.OutputTokens), CachedTokens: int(e.Usage.CachedTokens), Cost: e.Usage.Cost, TurnCount: int(e.Usage.TurnCount)}
	case *session.AttemptFinished:
		p.observeRuntimeSequence(e.Attempt.TurnID, e.Attempt.StepID)
		p.Usage.InputTokens += int(e.Attempt.Usage.InputTokens)
		p.Usage.OutputTokens += int(e.Attempt.Usage.OutputTokens)
		p.Usage.CachedTokens += int(e.Attempt.Usage.CachedTokens)
		p.Usage.Cost += e.Attempt.Usage.Cost
		p.Usage.TurnCount++
	case *session.TurnStarted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
	case *session.TurnFinished:
		p.observeRuntimeSequence(e.TurnID, "")
	case *session.RequestPrepared:
		p.observeRuntimeSequence(e.Manifest.TurnID, e.Manifest.StepID)
	case *session.ApprovalRequested, *session.ApprovalResolved, *session.HookStarted, *session.HookFinished, *session.ConversationRewound:
		// These facts do not currently have fields in SessionSnapshot.  They
		// remain available to richer projections and are intentionally not
		// guessed into history.
	case *session.ToolsDiscovered:
		if e.CatalogVersion > p.CatalogVersion {
			p.CatalogVersion = e.CatalogVersion
		}
		for _, tool := range e.Tools {
			p.Discovered[tool.ToolID] = tool
		}
	}
	p.Pending = p.Pending[:0]
	for _, inputID := range p.pendingOrder {
		if input, ok := p.pendingInput[inputID]; ok && input.Text != "" {
			p.Pending = append(p.Pending, input.Text)
		}
	}
	return nil
}

func (p *replayProjection) removePendingID(id string) {
	for i, current := range p.pendingOrder {
		if current == id {
			copy(p.pendingOrder[i:], p.pendingOrder[i+1:])
			p.pendingOrder = p.pendingOrder[:len(p.pendingOrder)-1]
			return
		}
	}
}

// persistLegacySnapshot is kept separate from List/Replay so read-only
// callers can never trigger import or directory creation.
func persistLegacySnapshot(cfg *config.Config, snapshot SessionSnapshot, sourcePath string) error {
	p, err := importLegacySnapshot(cfg, snapshot, sourcePath)
	if err != nil {
		return err
	}
	return p.close()
}

// prepareResumePersistence is the explicit import boundary used by Resume.
// New() alone never reads a legacy file, so opening a session by ID cannot
// silently migrate or merge two histories.
func prepareResumePersistence(cfg *config.Config, snapshot SessionSnapshot) error {
	if cfg == nil || cfg.NoSessionPersistence {
		return nil
	}
	newEvents := filepath.Join(cfg.SessionDir, snapshot.ID, "events.v1.jsonl")
	if _, err := os.Stat(newEvents); err == nil {
		store, openErr := session.OpenJSONLReadOnly(cfg.SessionDir, snapshot.ID)
		if openErr != nil {
			return openErr
		}
		records, readErr := session.ReadAll(store)
		_ = store.Close()
		if readErr != nil {
			return readErr
		}
		if !legacyImportEvidence(records) || legacyImportCompletePresent(records) {
			return nil
		}
		// The target is a recognized incomplete import. Continue with the same
		// deterministic transaction IDs below; Store deduplicates chunks already
		// synced before the crash.
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sourcePath := filepath.Join(cfg.SessionDir, snapshot.ID+".json")
	if _, err := os.Stat(sourcePath); errors.Is(err, os.ErrNotExist) {
		sourcePath = ""
	} else if err != nil {
		return err
	}
	return persistLegacySnapshot(cfg, snapshot, sourcePath)
}
