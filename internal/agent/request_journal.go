package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

const (
	requestWireFormat           = "llm.completion_request.v1"
	requestAdapterVersion       = "ccdp-llm-completion-v1"
	requestWireMaxBytes   int64 = session.DefaultMaxBlobBytes
)

var (
	ErrPreparedRequestNotFound       = errors.New("agent: prepared request not found")
	ErrPreparedRequestNotRebuildable = errors.New("agent: prepared request is not rebuildable")
	ErrPreparedRequestBlobMissing    = errors.New("agent: prepared request blob is missing")
	ErrPreparedRequestBlobCorrupt    = errors.New("agent: prepared request blob is corrupt")
)

// requestAttempt is the opaque hand-off between the provider call site and
// finishRequestAttempt. The manifest is retained for diagnostics, while
// finishRequestAttempt reloads the durable copy before committing an outcome.
// That second lookup prevents a caller from changing the provider/model
// identity in a copied value after admission.
type requestAttempt struct {
	AttemptID string
	RequestID string
	Manifest  session.RequestManifest

	// These values are captured while RequestPrepared is admitted. They are
	// deliberately not looked up again in finishRequestAttempt: model pricing,
	// credentials used only for diagnostic redaction, and the prompt baseline
	// must describe the same frozen step even if config/history changes while
	// the provider is streaming.
	price         config.Pricing
	priceCaptured bool
	apiKey        string
	historyLen    int
}

// preparedRequest is the internal verified representation used by finish and
// the Agent-local read-only loader. The public loader returns only the typed
// request; callers must not be able to mutate a manifest and then feed it back
// into the journal.
type preparedRequest struct {
	Manifest session.RequestManifest
	Request  llm.CompletionRequest
	Wire     []byte
}

type requestBlobStore interface {
	PutContext(context.Context, []byte) (session.BlobRef, error)
	Read(session.BlobRef, int64) ([]byte, error)
}

// memoryRequestArtifacts is the blob half of --no-session-persistence. It is
// intentionally independent of ArtifactStore because that type is
// filesystem-backed by design. Hash and size are checked both on write and
// read so memory mode has the same reconstruction contract as disk mode.
type memoryRequestArtifacts struct {
	mu       sync.RWMutex
	maxBytes int64
	blobs    map[string][]byte
}

func newMemoryRequestArtifacts(maxBytes int64) *memoryRequestArtifacts {
	if maxBytes <= 0 {
		maxBytes = requestWireMaxBytes
	}
	return &memoryRequestArtifacts{maxBytes: maxBytes, blobs: make(map[string][]byte)}
}

func (s *memoryRequestArtifacts) PutContext(ctx context.Context, data []byte) (session.BlobRef, error) {
	if s == nil {
		return session.BlobRef{}, errors.New("agent: nil memory request artifacts")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return session.BlobRef{}, err
	}
	if s.maxBytes > 0 && int64(len(data)) > s.maxBytes {
		return session.BlobRef{}, fmt.Errorf("%w: request wire exceeds %d bytes", session.ErrTooLarge, s.maxBytes)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	copyData := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.blobs[hash]; ok && !equalBytes(prior, copyData) {
		return session.BlobRef{}, fmt.Errorf("%w: memory blob hash collision", ErrPreparedRequestBlobCorrupt)
	}
	s.blobs[hash] = copyData
	return session.BlobRef{Hash: hash, Size: int64(len(copyData)), MediaType: "application/json"}, nil
}

func (s *memoryRequestArtifacts) Read(ref session.BlobRef, maxBytes int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("agent: nil memory request artifacts")
	}
	if err := validateRequestBlobRef(ref); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreparedRequestBlobCorrupt, err)
	}
	if maxBytes <= 0 {
		maxBytes = s.maxBytes
	}
	if maxBytes > 0 && ref.Size > maxBytes {
		return nil, fmt.Errorf("%w: request wire exceeds %d bytes", session.ErrTooLarge, maxBytes)
	}
	s.mu.RLock()
	data, ok := s.blobs[ref.Hash]
	copyData := append([]byte(nil), data...)
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPreparedRequestBlobMissing, ref.Hash)
	}
	if int64(len(copyData)) != ref.Size || (maxBytes > 0 && int64(len(copyData)) > maxBytes) {
		return nil, fmt.Errorf("%w: %s size changed", ErrPreparedRequestBlobCorrupt, ref.Hash)
	}
	sum := sha256.Sum256(copyData)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.Hash) {
		return nil, fmt.Errorf("%w: %s hash mismatch", ErrPreparedRequestBlobCorrupt, ref.Hash)
	}
	return copyData, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validateRequestBlobRef(ref session.BlobRef) error {
	if len(ref.Hash) != 64 {
		return errors.New("blob hash must be a SHA-256 hex string")
	}
	for _, r := range ref.Hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return errors.New("blob hash is not hexadecimal")
		}
	}
	if ref.Size < 0 {
		return errors.New("blob size must not be negative")
	}
	return nil
}

func (p *sessionPersistence) requestArtifacts() (requestBlobStore, error) {
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
	if p.artifacts != nil {
		return p.artifacts, nil
	}
	if p.memoryArtifacts == nil {
		p.memoryArtifacts = newMemoryRequestArtifacts(requestWireMaxBytes)
	}
	return p.memoryArtifacts, nil
}

func (p *sessionPersistence) poisonRequestJournal(err error) {
	if p == nil || err == nil {
		return
	}
	p.mu.Lock()
	if p.failed == nil {
		p.failed = err
	}
	p.mu.Unlock()
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
	return stableID(prefix, struct {
		Now int64
		Seq uint64
	}{time.Now().UTC().UnixNano(), seq})
}

func capturedRequestPricing(cfg *config.Config, model string) config.Pricing {
	if cfg == nil {
		return config.Pricing{}
	}
	return cfg.PricingFor(model)
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
	if a.cfg == nil {
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

	// Serialize the source revision capture, blob publication, and event
	// commit with Save and the other Agent projection writers. The revision is
	// captured before any settings update can race this admission.
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	a.mu.Lock()
	sessionID := a.sessionID
	turnSeq := a.turnSeq
	stepSeq := a.stepSeq
	sourceRevision := a.settingsRev
	price := capturedRequestPricing(a.cfg, req.Model)
	_, apiKey := a.cfg.EndpointFor(req.Model)
	historyLen := len(a.history)
	a.mu.Unlock()
	if metadata, ok := requestJournalMetadataFrom(ctx); ok {
		// Provider callers pass the immutable step snapshot through context.
		// Prefer it over Agent state so a concurrent /config or model switch
		// cannot rewrite the request's source revision, price card or redaction
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
		price = capturedRequestPricing(&metadata.Config, req.Model)
		_, apiKey = metadata.Config.EndpointFor(req.Model)
	}
	if sessionID == "" {
		sessionID = p.sessionID
	}
	if sourceRevision == 0 {
		sourceRevision = 1
	}
	turnID := fmt.Sprintf("turn-%d", turnSeq)
	stepID := fmt.Sprintf("step-%d", stepSeq)
	requestID := newRequestJournalID("request")
	manifest := session.RequestManifest{
		RequestID: requestID, SessionID: sessionID, TurnID: turnID, StepID: stepID,
		OperationID: fmt.Sprintf("%s/%s", turnID, stepID), Purpose: purpose,
		Model: req.Model, Provider: providerName, Endpoint: safeEndpoint,
		AdapterVersion: requestAdapterVersion,
		WireFormat:     requestWireFormat, Digest: digest,
		SourceRevision: sourceRevision,
	}
	artifacts, err := p.requestArtifacts()
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
		return fail(fmt.Errorf("agent: verify prepared request blob: %w", classifyRequestBlobError(err)))
	}
	if !equalBytes(storedWire, wire) {
		return fail(fmt.Errorf("%w: prepared request blob bytes changed", ErrPreparedRequestBlobCorrupt))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: "request-prepared-" + requestID,
		Events:        []session.Event{session.RequestPrepared{Manifest: manifest}},
	}); err != nil {
		return fail(fmt.Errorf("agent: persist RequestPrepared: %w", err))
	}
	return requestAttempt{
		AttemptID: newRequestJournalID("attempt"), RequestID: requestID, Manifest: manifest,
		price: price, priceCaptured: true, apiKey: apiKey, historyLen: historyLen,
	}, nil
}

func isRequestContextAbort(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func requestAttemptCost(attempt requestAttempt, inputTokens, outputTokens int) float64 {
	price := attempt.price
	if !attempt.priceCaptured {
		// A zero-value requestAttempt cannot have come from
		// recordPreparedRequest. Keep this fallback for package-local recovery
		// callers, but normal provider paths always use the captured card above.
		return 0
	}
	return float64(inputTokens)/1e6*price.Input + float64(outputTokens)/1e6*price.Output
}

// finishRequestAttempt records one complete Provider.Stream invocation. HTTP
// retries hidden inside that invocation never call this method and therefore
// never create additional AttemptFinished facts.
func (a *Agent) finishRequestAttempt(attempt requestAttempt, result llm.StreamResult, callErr error) error {
	if a == nil {
		return errors.New("agent: nil agent")
	}
	if strings.TrimSpace(attempt.AttemptID) == "" || strings.TrimSpace(attempt.RequestID) == "" {
		return a.poisonRequestJournal(errors.New("agent: request attempt identity is required"))
	}
	if result.PromptTokens < 0 || result.CompletionTok < 0 || result.CachedTokens < 0 {
		return a.poisonRequestJournal(errors.New("agent: provider usage must not be negative"))
	}
	a.persistMu.Lock()
	if err := a.persistenceFailure(); err != nil {
		a.persistMu.Unlock()
		return err
	}
	p := a.persistenceHandle()
	if p == nil {
		err := a.poisonRequestJournal(errors.New("agent: session persistence is unavailable"))
		a.persistMu.Unlock()
		return err
	}
	prepared, err := p.loadPreparedRequest(attempt.RequestID)
	if err != nil {
		err = a.poisonRequestJournal(err)
		a.persistMu.Unlock()
		return err
	}
	if prepared.Manifest.SessionID != "" && p.sessionID != "" && prepared.Manifest.SessionID != p.sessionID {
		err := a.poisonRequestJournal(errors.New("agent: prepared request session mismatch"))
		a.persistMu.Unlock()
		return err
	}
	inputTokens, outputTokens, cachedTokens := result.PromptTokens, result.CompletionTok, result.CachedTokens
	outcome := "success"
	if callErr != nil {
		if isRequestContextAbort(callErr) {
			outcome = "cancelled"
		} else {
			outcome = "error"
		}
	}
	candidate, cost, usageErr := a.accumulateAttemptUsage(inputTokens, outputTokens, cachedTokens, attempt)
	if usageErr != nil {
		err := a.poisonRequestJournal(usageErr)
		a.persistMu.Unlock()
		return err
	}
	cache := protocol.CacheStats{Tracking: true, InputTokens: inputTokens}
	if result.CacheReported && cachedTokens >= 0 && cachedTokens <= inputTokens {
		cache.CachedTokens = cachedTokens
		cache.ReportedInputTokens = inputTokens
	}
	if !candidate.Cache.Tracking && candidate.TurnCount > 1 {
		candidate.Cache.UnknownHistory = true
	}
	candidate.Cache.Tracking = true
	candidate.Cache.InputTokens += cache.InputTokens
	candidate.Cache.CachedTokens += cache.CachedTokens
	candidate.Cache.ReportedInputTokens += cache.ReportedInputTokens
	usage := session.Usage{
		Cache:       cache,
		InputTokens: int64(inputTokens), OutputTokens: int64(outputTokens),
		CachedTokens: int64(cachedTokens), TotalTokens: int64(inputTokens + outputTokens),
		Cost: cost, TurnCount: 1,
	}
	finishedAt := time.Now().UTC()
	attemptEvent := session.AttemptFinished{Attempt: session.Attempt{
		AttemptID: attempt.AttemptID, RequestID: attempt.RequestID,
		SessionID: prepared.Manifest.SessionID, TurnID: prepared.Manifest.TurnID,
		StepID: prepared.Manifest.StepID, Purpose: prepared.Manifest.Purpose,
		Provider: prepared.Manifest.Provider, Model: prepared.Manifest.Model,
		Endpoint: prepared.Manifest.Endpoint, Outcome: outcome,
		Error: safeAttemptError(callErr, attempt.apiKey, prepared.Manifest.Endpoint),
		Usage: usage, FinishedAt: finishedAt,
	}}
	absolute := session.Usage{
		InputTokens: int64(candidate.InputTokens), OutputTokens: int64(candidate.OutputTokens),
		CachedTokens: int64(candidate.CachedTokens), Cache: candidate.Cache, TotalTokens: int64(candidate.InputTokens + candidate.OutputTokens),
		Cost: candidate.Cost, TurnCount: int64(candidate.TurnCount),
	}
	revision := uint64(p.CurrentCursor() + 1)
	if _, err := p.Commit(session.Batch{
		TransactionID: "attempt-finished-" + attempt.AttemptID,
		Events:        []session.Event{attemptEvent, session.UsageChanged{Revision: revision, Usage: absolute}},
	}); err != nil {
		err = a.poisonRequestJournal(fmt.Errorf("agent: persist AttemptFinished: %w", err))
		a.persistMu.Unlock()
		return err
	}
	a.mu.Lock()
	a.usage = candidate
	// Main requests establish the same prompt-token baseline the old usage
	// path established. Guardian/sub-agent callers can retain their separate
	// history baseline by using their own purpose.
	if callErr == nil && (prepared.Manifest.Purpose == "main" || prepared.Manifest.Purpose == "chat" || prepared.Manifest.Purpose == "completion" || prepared.Manifest.Purpose == "task" || prepared.Manifest.Purpose == "guardian") {
		a.tokenBaseline.promptTokens = inputTokens
		a.tokenBaseline.historyLen = attempt.historyLen
	}
	a.mu.Unlock()
	// Commit and the live projection are now complete. Queue the internal
	// compatibility event while this ordered journal section is still held,
	// then release before the synchronous external bus below: a subscriber may
	// call Save or another persistence operation, and re-entering the journal
	// while persistMu is held would deadlock.
	a.emit(Event{Type: EventUsage, Usage: &candidate})
	a.persistMu.Unlock()
	if a.evbus != nil {
		a.evbus.Emit(events.TopicUsageUpdated, events.UsageEvent{
			InputTokens: candidate.InputTokens, OutputTokens: candidate.OutputTokens,
			TurnCount: candidate.TurnCount, Cost: candidate.Cost,
		})
	}
	return nil
}

// accumulateAttemptUsage folds a finished attempt's tokens into the session
// usage counter, rejecting non-finite costs. The caller holds persistMu so the
// counter update is serialized with the durable journal section.
func (a *Agent) accumulateAttemptUsage(inputTokens, outputTokens, cachedTokens int, attempt requestAttempt) (candidate Usage, cost float64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.usage
	cost = requestAttemptCost(attempt, inputTokens, outputTokens)
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return current, cost, errors.New("agent: provider usage cost is not finite")
	}
	candidate = current
	candidate.InputTokens += inputTokens
	candidate.OutputTokens += outputTokens
	candidate.CachedTokens += cachedTokens
	candidate.TurnCount++
	candidate.Cost += cost
	if math.IsNaN(candidate.Cost) || math.IsInf(candidate.Cost, 0) {
		return current, cost, errors.New("agent: accumulated usage cost is not finite")
	}
	return candidate, cost, nil
}

func (p *sessionPersistence) loadPreparedRequest(requestID string) (preparedRequest, error) {
	if p == nil {
		return preparedRequest{}, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return preparedRequest{}, session.ErrClosed
	}
	var blobs requestBlobStore = p.memoryArtifacts
	if p.artifacts != nil {
		blobs = p.artifacts
	}
	prepared, err := loadPreparedRequestLocked(p.store, blobs, requestID)
	if err == nil && prepared.Manifest.SessionID != "" && p.sessionID != "" && prepared.Manifest.SessionID != p.sessionID {
		return preparedRequest{}, fmt.Errorf("%w: prepared request session mismatch", ErrPreparedRequestBlobCorrupt)
	}
	return prepared, err
}

func loadPreparedRequestLocked(store session.ReadOnlyStore, blobs requestBlobStore, requestID string) (preparedRequest, error) {
	if store == nil {
		return preparedRequest{}, errors.New("agent: prepared request store is unavailable")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return preparedRequest{}, errors.New("agent: prepared request ID is required")
	}
	records, err := session.ReadAll(store)
	if err != nil {
		return preparedRequest{}, err
	}
	var manifest *session.RequestManifest
	for _, record := range records {
		var candidate *session.RequestManifest
		switch event := record.Event.(type) {
		case *session.RequestPrepared:
			candidate = &event.Manifest
		case session.RequestPrepared:
			copyManifest := event.Manifest
			candidate = &copyManifest
		}
		if candidate == nil || candidate.RequestID != requestID {
			continue
		}
		if manifest != nil {
			return preparedRequest{}, fmt.Errorf("%w: duplicate RequestPrepared %q", ErrPreparedRequestBlobCorrupt, requestID)
		}
		copyManifest := *candidate
		manifest = &copyManifest
	}
	if manifest == nil {
		return preparedRequest{}, fmt.Errorf("%w: %s", ErrPreparedRequestNotFound, requestID)
	}
	if manifest.WireBody == nil {
		return preparedRequest{}, fmt.Errorf("%w: request %s has no supported wire body", ErrPreparedRequestNotRebuildable, requestID)
	}
	if manifest.WireFormat != requestWireFormat {
		return preparedRequest{}, fmt.Errorf("%w: request %s has unsupported wire format", ErrPreparedRequestNotRebuildable, requestID)
	}
	if blobs == nil {
		return preparedRequest{}, fmt.Errorf("%w: %s", ErrPreparedRequestBlobMissing, manifest.WireBody.Hash)
	}
	wire, err := blobs.Read(*manifest.WireBody, requestWireMaxBytes)
	if err != nil {
		return preparedRequest{}, classifyRequestBlobError(err)
	}
	sum := sha256.Sum256(wire)
	digest := hex.EncodeToString(sum[:])
	if !strings.EqualFold(digest, manifest.WireBody.Hash) || int64(len(wire)) != manifest.WireBody.Size ||
		!strings.EqualFold(digest, manifest.Digest) {
		return preparedRequest{}, fmt.Errorf("%w: request %s digest mismatch", ErrPreparedRequestBlobCorrupt, requestID)
	}
	var req llm.CompletionRequest
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		return preparedRequest{}, fmt.Errorf("%w: request %s JSON: %v", ErrPreparedRequestBlobCorrupt, requestID, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return preparedRequest{}, fmt.Errorf("%w: request %s has trailing JSON", ErrPreparedRequestBlobCorrupt, requestID)
		}
		return preparedRequest{}, fmt.Errorf("%w: request %s trailing JSON: %v", ErrPreparedRequestBlobCorrupt, requestID, err)
	}
	if !req.Stream || req.Model == "" || req.Model != manifest.Model {
		return preparedRequest{}, fmt.Errorf("%w: request %s body identity mismatch", ErrPreparedRequestBlobCorrupt, requestID)
	}
	return preparedRequest{Manifest: *manifest, Request: req, Wire: append([]byte(nil), wire...)}, nil
}

func classifyRequestBlobError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrPreparedRequestBlobMissing) || errors.Is(err, ErrPreparedRequestBlobCorrupt) || errors.Is(err, session.ErrTooLarge) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrPreparedRequestBlobMissing, err)
	}
	return fmt.Errorf("%w: %v", ErrPreparedRequestBlobCorrupt, err)
}

// LoadPreparedRequest loads and verifies a frozen request from a JSONL
// session directory. Legacy sessions without RequestPrepared/wire facts are
// reported unavailable rather than being given a fabricated request.
func LoadPreparedRequest(sessionRoot, sessionID, requestID string) (llm.CompletionRequest, error) {
	store, err := session.OpenJSONLReadOnly(sessionRoot, sessionID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	defer store.Close()
	blobs := session.OpenArtifactStoreReadOnly(store.Dir())
	prepared, err := loadPreparedRequestLocked(store, blobs, requestID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	if prepared.Manifest.SessionID != "" && prepared.Manifest.SessionID != sessionID {
		return llm.CompletionRequest{}, fmt.Errorf("%w: prepared request session mismatch", ErrPreparedRequestBlobCorrupt)
	}
	return prepared.Request, nil
}

func (a *Agent) LoadPreparedRequest(requestID string) (llm.CompletionRequest, error) {
	if a == nil {
		return llm.CompletionRequest{}, errors.New("agent: nil agent")
	}
	p := a.persistenceHandle()
	if p == nil {
		return llm.CompletionRequest{}, errors.New("agent: session persistence is unavailable")
	}
	prepared, err := p.loadPreparedRequest(requestID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	return prepared.Request, nil
}

var (
	requestAuthErrorRE = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?|api[_-]?key\s*[:=]\s*|token\s*[:=]\s*)([^\s,;"'}]+)`)
	requestURLRE       = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
)

func safeAttemptError(err error, secret, endpoint string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	// Do not persist URL userinfo or query strings from url.Error. Endpoint
	// itself has already been stripped in the manifest; this handles provider
	// diagnostics that repeat the original unsanitized URL.
	s = requestURLRE.ReplaceAllStringFunc(s, func(rawURL string) string {
		parsed, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return "[redacted-url]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
	if endpoint != "" {
		if parsed, parseErr := url.Parse(endpoint); parseErr == nil {
			parsed.User = nil
			parsed.RawQuery = ""
			parsed.ForceQuery = false
			parsed.Fragment = ""
			s = strings.ReplaceAll(s, endpoint, parsed.String())
		}
	}
	s = requestAuthErrorRE.ReplaceAllString(s, `${1}[redacted]`)
	if len(s) > 4096 {
		s = s[:4096] + "…"
	}
	return s
}

func sanitizeRequestEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("agent: invalid provider endpoint: %w", err)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	result := strings.TrimSpace(u.String())
	if strings.ContainsAny(result, "\r\n") {
		return "", errors.New("agent: provider endpoint contains a newline")
	}
	return result, nil
}
