package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/session/journal"
)

const (
	requestWireFormat           = journal.RequestWireFormat
	requestAdapterVersion       = "ccdp-llm-completion-v1"
	requestWireMaxBytes   int64 = session.DefaultMaxBlobBytes
)

var (
	ErrPreparedRequestNotFound       = archive.ErrPreparedRequestNotFound
	ErrPreparedRequestNotRebuildable = archive.ErrPreparedRequestNotRebuildable
	ErrPreparedRequestBlobMissing    = journal.ErrBlobMissing
	ErrPreparedRequestBlobCorrupt    = journal.ErrBlobCorrupt
)

// requestAttempt is the opaque hand-off between the provider call site and
// finishRequestAttempt. Completion uses the persistence-owned index of
// committed request metadata; caller mutations cannot rewrite that identity.
// Full wire reconstruction is reserved for explicit diagnostic reads.
type requestAttempt struct {
	goalOwner      goalAccountTarget
	goalID         string
	estimatedInput int
	AttemptID      string
	RequestID      string
	Manifest       session.RequestManifest

	// These values are captured while RequestPrepared is admitted. They are
	// deliberately not looked up again in finishRequestAttempt: credentials
	// used only for diagnostic redaction and the prompt baseline must describe the same frozen step even if config/history changes while
	// the provider is streaming.
	apiKey     string
	historyLen int
}

func (p *sessionPersistence) poisonRequestJournal(err error) {
	var writer *journal.Writer
	if p != nil {
		writer = p.Writer
	}
	writer.PoisonRequestJournal(err)
}

func (a *Agent) poisonRequestJournal(err error) error {
	if err == nil {
		return nil
	}
	if a != nil {
		if p := a.persistenceHandle(); p != nil {
			p.poisonRequestJournal(err)
		}
		a.markPersistenceFailure(err)
	}
	return err
}

var requestIDCounter uint64

func newRequestJournalID(prefix string) string {
	seq := atomic.AddUint64(&requestIDCounter, 1)
	return messages.StableID(prefix, struct {
		Now int64
		Seq uint64
	}{time.Now().UTC().UnixNano(), seq})
}

// recordPreparedRequest durably records the exact request body before the
// provider call begins. req is already frozen by the provider adapter and
// must carry Stream=true; this method never re-encodes or edits its prompt.
func (a *Agent) recordPreparedRequest(ctx context.Context, purpose, providerName, endpoint string, req llm.CompletionRequest) (requestAttempt, error) {
	var zero requestAttempt
	if a == nil {
		return zero, errors.New("agent: nil agent")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	goalOwner, goalID := a.goalAccount()
	goalSessionID := ""
	if goalOwner != nil {
		goalSessionID = goalOwner.SessionID()
	}
	p := a.persistenceHandle()
	fail := func(err error) (requestAttempt, error) {
		if isRequestContextAbort(err) {
			// A caller cancelling a preparation before the provider starts is a
			// normal control-flow outcome. No durable fact was accepted, so it
			// must not permanently poison an otherwise healthy session.
			return zero, err
		}
		return zero, a.poisonRequestJournal(err)
	}
	if p == nil {
		return fail(errors.New("agent: session persistence is unavailable"))
	}
	if a.services.cfg == nil {
		return fail(errors.New("agent: config is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if !req.Stream {
		return fail(errors.New("agent: prepared request must have stream=true"))
	}
	if strings.TrimSpace(req.Model) == "" {
		return fail(errors.New("agent: prepared request model is required"))
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return fail(errors.New("agent: prepared request provider is required"))
	}
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return fail(errors.New("agent: prepared request purpose is required"))
	}
	safeEndpoint, err := sanitizeRequestEndpoint(endpoint)
	if err != nil {
		return fail(err)
	}
	wire, err := json.Marshal(req)
	if err != nil {
		return fail(fmt.Errorf("agent: marshal prepared request: %w", err))
	}
	if len(wire) == 0 || int64(len(wire)) > requestWireMaxBytes {
		return fail(fmt.Errorf("%w: request wire exceeds %d bytes", session.ErrTooLarge, requestWireMaxBytes))
	}
	sum := sha256.Sum256(wire)
	digest := hex.EncodeToString(sum[:])

	// Freeze source metadata before artifact I/O. The owner will admit this
	// exact manifest; a later settings change cannot rewrite the wire identity.
	a.mu.Lock()
	sessionID := a.conversation.sessionID
	turnSeq := a.sequences.turnSeq
	stepSeq := a.sequences.stepSeq
	sourceRevision := a.sequences.settingsRev
	reservedOutput := a.services.cfg.MaxOutputTokensFor(req.Model)
	_, apiKey := a.services.cfg.EndpointFor(req.Model)
	historyLen := len(a.historyLocked())
	a.mu.Unlock()
	if metadata, ok := requestJournalMetadataFrom(ctx); ok {
		// Provider callers pass the immutable step snapshot through context.
		// Prefer it over Agent state so a concurrent /config or model switch
		// cannot rewrite the request's source revision or redaction
		// key after the adapter froze the body.
		if metadata.SettingsRevision != 0 {
			sourceRevision = metadata.SettingsRevision
		} else if metadata.ContextRevision != 0 {
			sourceRevision = metadata.ContextRevision
		}
		turnSeq = metadata.Turn
		stepSeq = metadata.Step
		historyLen = metadata.HistoryLen
		if historyLen < 0 {
			return fail(errors.New("agent: request history length must not be negative"))
		}
		reservedOutput = metadata.Config.MaxOutputTokensFor(req.Model)
		_, apiKey = metadata.Config.EndpointFor(req.Model)
	}
	if sessionID == "" {
		sessionID = p.Identity()
	}
	if sourceRevision == 0 {
		sourceRevision = 1
	}
	turnID := fmt.Sprintf("turn-%d", turnSeq)
	stepID := fmt.Sprintf("step-%d", stepSeq)
	requestID := newRequestJournalID("request")
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		reservedOutput = *req.MaxTokens
	}
	reservedInput := len(wire)
	reservedUsage := session.Usage{InputTokens: int64(reservedInput), OutputTokens: int64(reservedOutput), TotalTokens: int64(reservedInput) + int64(reservedOutput), TurnCount: 1}
	manifest := session.RequestManifest{
		ReservedUsage: &reservedUsage,
		GoalSessionID: goalSessionID, GoalID: goalID,
		RequestID: requestID, SessionID: sessionID, TurnID: turnID, StepID: stepID,
		OperationID: fmt.Sprintf("%s/%s", turnID, stepID), Purpose: purpose,
		Model: req.Model, Provider: providerName, Endpoint: safeEndpoint,
		AdapterVersion: requestAdapterVersion,
		WireFormat:     requestWireFormat, Digest: digest,
		SourceRevision: sourceRevision,
	}
	artifacts, err := p.RequestArtifacts()
	if err != nil {
		return fail(err)
	}
	ref, err := artifacts.PutContext(ctx, wire)
	if err != nil {
		return fail(fmt.Errorf("agent: persist prepared request blob: %w", err))
	}
	if !strings.EqualFold(ref.Hash, digest) || ref.Size != int64(len(wire)) {
		return fail(fmt.Errorf("%w: prepared request blob identity mismatch", ErrPreparedRequestBlobCorrupt))
	}
	manifest.WireBody = &ref
	storedWire, err := artifacts.Read(ref, requestWireMaxBytes)
	if err != nil {
		return fail(fmt.Errorf("agent: verify prepared request blob: %w", archive.ClassifyRequestBlobError(err)))
	}
	if !bytes.Equal(storedWire, wire) {
		return fail(fmt.Errorf("%w: prepared request blob bytes changed", ErrPreparedRequestBlobCorrupt))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := a.admitPreparedRequest(ctx, manifest); err != nil {
		return fail(err)
	}
	return requestAttempt{
		goalOwner: goalOwner, goalID: goalID, estimatedInput: llm.EstimateTokens(string(wire)),
		AttemptID: newRequestJournalID("attempt"), RequestID: requestID, Manifest: manifest,
		apiKey: apiKey, historyLen: historyLen,
	}, nil
}

func isRequestContextAbort(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
