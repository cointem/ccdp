package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"ccdp/internal/atomicfile"
)

const (
	// SchemaVersion is the only JSONL format this implementation writes.
	SchemaVersion uint32 = 1
	// DefaultMaxTransactionBytes prevents a JSONL line from becoming an
	// accidental unbounded allocation.
	DefaultMaxTransactionBytes int64 = 8 << 20
	// DefaultMaxEventsPerTransaction keeps transaction admission bounded.
	DefaultMaxEventsPerTransaction = 256
	// DefaultMaxBlobBytes bounds one content-addressed artifact.
	DefaultMaxBlobBytes int64 = 64 << 20
)

// Cursor is the monotonically increasing sequence watermark for one session.
// It is an alias rather than a distinct integer type so runtime code can pass
// a persisted uint64 without conversion.  Cursor 0 denotes the empty log.
type Cursor = uint64

const (
	Beginning Cursor = 0
	// AnyCursor skips the optimistic-concurrency check.  It is intended for
	// controlled replay/import paths, not normal runtime admission.
	AnyCursor Cursor = ^Cursor(0)
)

var (
	ErrClosed            = errors.New("session: store is closed")
	ErrReadOnly          = errors.New("session: store is read-only")
	ErrWriterLocked      = errors.New("session: another writer owns this session")
	ErrCursorConflict    = errors.New("session: expected cursor does not match")
	ErrDuplicateID       = errors.New("session: duplicate id has different content")
	ErrFutureVersion     = errors.New("session: future storage version")
	ErrCorrupt           = errors.New("session: corrupt event log")
	ErrTorn              = errors.New("session: incomplete event-log tail")
	ErrTooLarge          = errors.New("session: content exceeds configured limit")
	ErrPersistenceFailed = errors.New("session: persistence failed")
	ErrSnapshotNotFound  = errors.New("session: snapshot cache not found")
	ErrSnapshotInvalid   = errors.New("session: invalid snapshot cache")
	ErrUnknownEventType  = errors.New("session: unknown event type")
	ErrInvalidSessionID  = errors.New("session: invalid session id")
)

// CursorConflictError preserves both values for a structured command
// response.  errors.Is(err, ErrCursorConflict) remains true.
type CursorConflictError struct {
	Expected Cursor
	Actual   Cursor
}

func (e *CursorConflictError) Error() string {
	return fmt.Sprintf("%v: expected %d, current %d", ErrCursorConflict, e.Expected, e.Actual)
}

func (e *CursorConflictError) Unwrap() error { return ErrCursorConflict }

// RecoveryReport describes a torn EOF tail repaired while opening a writer.
// The original bytes are kept in TailPath for diagnosis.  A read-only opener
// never returns a report because it never changes the log.
type RecoveryReport struct {
	Recovered bool
	TailPath  string
	TailBytes int64
}

// Batch is one atomic commit. TransactionID is optional; the writer creates a
// fresh ID when omitted. A caller retrying a commit should reuse the same ID
// so a lost receipt is idempotent.
type Batch struct {
	TransactionID string
	Events        []Event
}

func NewBatch(events ...Event) Batch { return Batch{Events: events} }

func (b Batch) id() (string, error) {
	return b.TransactionID, nil
}

// Record is a single event with its assigned durable sequence and transaction
// identity.  Event values returned by Read are deep copies of the store's
// internal values.
type Record struct {
	Seq           Cursor
	TransactionID string
	Event         Event
}

type CommitResult struct {
	TransactionID string
	FirstSeq      Cursor
	LastSeq       Cursor
	Cursor        Cursor
	Applied       bool
	Deduplicated  bool
}

// Snapshot is an optional acceleration cache.  It is never consulted by Read
// or replay, and callers can safely delete it and rebuild from the log.
// Data is opaque bytes owned by the projection layer, not an untyped event
// payload; JSONL stores it base64-encoded in snapshot.json.
type Snapshot struct {
	SchemaVersion uint32
	SessionID     string
	LastSeq       Cursor
	UpdatedAt     time.Time
	Data          []byte
}

// Store is the persistence contract shared by MemoryStore and JSONLStore.
// Commit is the only mutating business-fact operation.  Sequence allocation,
// event validation, transaction checksum and idempotency are store-owned.
type Store interface {
	Commit(expected Cursor, batch Batch) (CommitResult, error)
	Read(after Cursor) ([]Record, error)
	Snapshot() (Snapshot, error)
	SaveSnapshot(snapshot Snapshot) error
	CurrentCursor() Cursor
	Close() error
}

// ReadOnlyStore captures the operations safe for listing/replay consumers.
// JSONL read-only handles implement Store too, but return ErrReadOnly on
// mutating methods.
type ReadOnlyStore interface {
	Read(after Cursor) ([]Record, error)
	Snapshot() (Snapshot, error)
	CurrentCursor() Cursor
}

// JSONLOptions controls bounded admission and injectable sync hooks.  The
// hooks exist to make write/sync failures deterministic in conformance tests;
// nil uses real os.File.Sync and atomicfile.SyncDir.
type JSONLOptions struct {
	MaxTransactionBytes     int64
	MaxEventsPerTransaction int
	MaxBlobBytes            int64
	SyncFile                atomicfile.SyncFile
	SyncDir                 func(string) error
}

type JSONLOption func(*JSONLOptions)

func WithMaxTransactionBytes(n int64) JSONLOption {
	return func(o *JSONLOptions) { o.MaxTransactionBytes = n }
}

func WithMaxEventsPerTransaction(n int) JSONLOption {
	return func(o *JSONLOptions) { o.MaxEventsPerTransaction = n }
}

func WithMaxBlobBytes(n int64) JSONLOption {
	return func(o *JSONLOptions) { o.MaxBlobBytes = n }
}

func WithSyncFile(fn atomicfile.SyncFile) JSONLOption {
	return func(o *JSONLOptions) { o.SyncFile = fn }
}

func WithSyncDir(fn func(string) error) JSONLOption {
	return func(o *JSONLOptions) { o.SyncDir = fn }
}

func defaultJSONLOptions() JSONLOptions {
	return JSONLOptions{
		MaxTransactionBytes:     DefaultMaxTransactionBytes,
		MaxEventsPerTransaction: DefaultMaxEventsPerTransaction,
		MaxBlobBytes:            DefaultMaxBlobBytes,
	}
}

func applyJSONLOptions(options []JSONLOption) JSONLOptions {
	o := defaultJSONLOptions()
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	if o.MaxTransactionBytes <= 0 {
		o.MaxTransactionBytes = DefaultMaxTransactionBytes
	}
	if o.MaxEventsPerTransaction <= 0 {
		o.MaxEventsPerTransaction = DefaultMaxEventsPerTransaction
	}
	if o.MaxBlobBytes <= 0 {
		o.MaxBlobBytes = DefaultMaxBlobBytes
	}
	return o
}

type transactionEnvelope struct {
	SchemaVersion    uint32      `json:"schema_version"`
	TransactionID    string      `json:"transaction_id"`
	FirstSeq         Cursor      `json:"first_seq"`
	LastSeq          Cursor      `json:"last_seq"`
	Events           []wireEvent `json:"events"`
	InputFingerprint string      `json:"input_fingerprint"`
	Checksum         string      `json:"checksum"`
}

type transactionBody struct {
	SchemaVersion    uint32      `json:"schema_version"`
	TransactionID    string      `json:"transaction_id"`
	FirstSeq         Cursor      `json:"first_seq"`
	LastSeq          Cursor      `json:"last_seq"`
	Events           []wireEvent `json:"events"`
	InputFingerprint string      `json:"input_fingerprint"`
}

func (e transactionEnvelope) body() transactionBody {
	return transactionBody{
		SchemaVersion:    e.SchemaVersion,
		TransactionID:    e.TransactionID,
		FirstSeq:         e.FirstSeq,
		LastSeq:          e.LastSeq,
		Events:           e.Events,
		InputFingerprint: e.InputFingerprint,
	}
}

func checksumBody(body transactionBody) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func encodeTransaction(txID string, first Cursor, events []wireEvent, inputFingerprint string) ([]byte, transactionEnvelope, error) {
	if txID == "" {
		return nil, transactionEnvelope{}, errors.New("transaction ID is required")
	}
	if len(events) == 0 {
		return nil, transactionEnvelope{}, errors.New("transaction must contain an event")
	}
	if inputFingerprint == "" {
		return nil, transactionEnvelope{}, errors.New("transaction input fingerprint is required")
	}
	last := first + Cursor(len(events)) - 1
	envelope := transactionEnvelope{
		SchemaVersion:    SchemaVersion,
		TransactionID:    txID,
		FirstSeq:         first,
		LastSeq:          last,
		Events:           events,
		InputFingerprint: inputFingerprint,
	}
	var err error
	envelope.Checksum, err = checksumBody(envelope.body())
	if err != nil {
		return nil, transactionEnvelope{}, err
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		return nil, transactionEnvelope{}, err
	}
	return line, envelope, nil
}

func parseTransaction(data []byte) (parsedTransaction, error) {
	var envelope transactionEnvelope
	if err := decodeStrict(data, &envelope); err != nil {
		if isUnexpectedEOF(err) {
			return parsedTransaction{}, fmt.Errorf("%w: transaction JSON: %v", ErrTorn, err)
		}
		return parsedTransaction{}, fmt.Errorf("%w: transaction JSON: %v", ErrCorrupt, err)
	}
	if envelope.SchemaVersion > SchemaVersion {
		return parsedTransaction{}, fmt.Errorf("%w: transaction schema_version %d", ErrFutureVersion, envelope.SchemaVersion)
	}
	if envelope.SchemaVersion != SchemaVersion {
		return parsedTransaction{}, fmt.Errorf("%w: unsupported transaction schema_version %d", ErrCorrupt, envelope.SchemaVersion)
	}
	if envelope.TransactionID == "" || envelope.FirstSeq == 0 || envelope.LastSeq < envelope.FirstSeq || len(envelope.Events) == 0 || envelope.InputFingerprint == "" {
		return parsedTransaction{}, fmt.Errorf("%w: invalid transaction bounds or ID", ErrCorrupt)
	}
	if envelope.LastSeq-envelope.FirstSeq+1 != Cursor(len(envelope.Events)) {
		return parsedTransaction{}, fmt.Errorf("%w: transaction sequence count mismatch", ErrCorrupt)
	}
	checksum, err := checksumBody(envelope.body())
	if err != nil || !strings.EqualFold(checksum, envelope.Checksum) {
		if err != nil {
			return parsedTransaction{}, fmt.Errorf("%w: checksum: %v", ErrCorrupt, err)
		}
		return parsedTransaction{}, fmt.Errorf("%w: transaction checksum mismatch", ErrCorrupt)
	}
	parsed := parsedTransaction{
		envelope: envelope,
		events:   make([]Event, 0, len(envelope.Events)),
		wires:    make([]wireEvent, 0, len(envelope.Events)),
		seqs:     make([]Cursor, 0, len(envelope.Events)),
	}
	for i, wire := range envelope.Events {
		seq := envelope.FirstSeq + Cursor(i)
		event, err := decodeEvent(wire)
		if err != nil {
			if errors.Is(err, ErrUnknownEventType) {
				// Version skew, not damage: the record is intact and checksummed,
				// this build simply has no type for it. Count and skip it so a log
				// written by another build still resumes; everything else in the
				// transaction keeps its own sequence position.
				parsed.skipped = append(parsed.skipped, SkippedRecord{Seq: seq, Kind: wire.Type, Reason: err.Error()})
				continue
			}
			if isUnexpectedEOF(err) {
				return parsedTransaction{}, fmt.Errorf("%w: event %d: %v", ErrTorn, i, err)
			}
			return parsedTransaction{}, fmt.Errorf("%w: event %d: %v", ErrCorrupt, i, err)
		}
		parsed.events = append(parsed.events, event)
		parsed.wires = append(parsed.wires, wire)
		parsed.seqs = append(parsed.seqs, seq)
	}
	return parsed, nil
}

// parsedTransaction is one verified transaction line as replay sees it: the
// events this build can decode, each with the sequence number it occupies in
// the log, plus the records it had to skip.
type parsedTransaction struct {
	envelope transactionEnvelope
	events   []Event
	wires    []wireEvent
	seqs     []Cursor
	skipped  []SkippedRecord
}

// SkippedRecord names one durable event that replay could not decode because
// this build has no type for it. Skipping is confined to version skew; corrupt
// or torn records still fail the open.
type SkippedRecord struct {
	Seq    Cursor
	Kind   EventType
	Reason string
}

type storedTransaction struct {
	Result           CommitResult
	Fingerprint      string // fingerprint of the events actually persisted
	InputFingerprint string // fingerprint of the caller's complete batch
}

type dedupeEntry struct {
	Fingerprint string
	Result      CommitResult
}

type logState struct {
	Cursor       Cursor
	Records      []Record
	Transactions map[string]storedTransaction
	Dedupe       map[string]dedupeEntry
	// Skipped lists records replay could not decode because this build has no
	// event type for them. It is only ever filled while reading a log.
	Skipped []SkippedRecord
}

func newLogState() logState {
	return logState{
		Transactions: make(map[string]storedTransaction),
		Dedupe:       make(map[string]dedupeEntry),
	}
}

func transactionFingerprint(wires []wireEvent) string {
	data, _ := json.Marshal(wires)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type preparedCommit struct {
	TransactionID    string
	InputFingerprint string
	Events           []Event
	Wires            []wireEvent
	// Seqs gives each event its sequence number in the log. The live write path
	// leaves it nil, because there the events are contiguous from FirstSeq;
	// replay fills it in, because skipped records leave holes behind.
	Seqs []Cursor
	// Skipped carries the records replay dropped for version skew, so applying
	// the transaction also accounts for them.
	Skipped  []SkippedRecord
	Line     []byte
	Envelope transactionEnvelope
	Result   CommitResult
}

func prepareCommit(state *logState, expected Cursor, batch Batch, options JSONLOptions) (preparedCommit, error) {
	if len(batch.Events) == 0 {
		return preparedCommit{Result: CommitResult{Cursor: state.Cursor}}, nil
	}
	if len(batch.Events) > options.MaxEventsPerTransaction {
		return preparedCommit{}, fmt.Errorf("%w: %d events exceeds limit %d", ErrTooLarge, len(batch.Events), options.MaxEventsPerTransaction)
	}
	txID, err := batch.id()
	if err != nil {
		return preparedCommit{}, err
	}
	if txID == "" {
		txID = newID("tx")
	}
	// Normalize the complete caller batch before filtering idempotent event
	// IDs.  A retry must be compared with what the caller originally submitted,
	// even when the first attempt filtered an already durable event and only
	// persisted the remainder.
	inputEvents := make([]Event, 0, len(batch.Events))
	inputWires := make([]wireEvent, 0, len(batch.Events))
	for _, event := range batch.Events {
		normalized, wire, err := normalizeEvent(event)
		if err != nil {
			return preparedCommit{}, err
		}
		inputEvents = append(inputEvents, normalized)
		inputWires = append(inputWires, wire)
	}
	inputFingerprint := transactionFingerprint(inputWires)
	if previous, ok := state.Transactions[txID]; ok {
		previousInput := previous.InputFingerprint
		if previousInput == "" {
			previousInput = previous.Fingerprint
		}
		if inputFingerprint != previousInput {
			return preparedCommit{}, fmt.Errorf("%w: transaction %q", ErrDuplicateID, txID)
		}
		result := previous.Result
		result.Deduplicated = true
		result.Applied = false
		return preparedCommit{TransactionID: txID, InputFingerprint: inputFingerprint, Result: result}, nil
	}

	prepared := preparedCommit{TransactionID: txID, InputFingerprint: inputFingerprint}
	seenInBatch := make(map[string]string)
	for i, normalized := range inputEvents {
		wire := inputWires[i]
		key := eventDedupeKey(normalized)
		fingerprint := eventFingerprint(wire)
		if key != "" {
			if prior, ok := seenInBatch[key]; ok {
				if prior != fingerprint {
					return preparedCommit{}, fmt.Errorf("%w: %s", ErrDuplicateID, key)
				}
				continue
			}
			seenInBatch[key] = fingerprint
			if prior, ok := state.Dedupe[key]; ok {
				if prior.Fingerprint != fingerprint {
					return preparedCommit{}, fmt.Errorf("%w: %s", ErrDuplicateID, key)
				}
				continue
			}
		}
		prepared.Events = append(prepared.Events, normalized)
		prepared.Wires = append(prepared.Wires, wire)
	}
	if len(prepared.Events) == 0 {
		result := CommitResult{TransactionID: txID, Cursor: state.Cursor, Deduplicated: true}
		for key := range seenInBatch {
			if prior, ok := state.Dedupe[key]; ok {
				result.TransactionID = prior.Result.TransactionID
				result.FirstSeq = prior.Result.FirstSeq
				result.LastSeq = prior.Result.LastSeq
				break
			}
		}
		return preparedCommit{TransactionID: txID, InputFingerprint: inputFingerprint, Result: result}, nil
	}
	if expected != AnyCursor && expected != state.Cursor {
		return preparedCommit{}, &CursorConflictError{Expected: expected, Actual: state.Cursor}
	}
	first := state.Cursor + 1
	line, envelope, err := encodeTransaction(txID, first, prepared.Wires, prepared.InputFingerprint)
	if err != nil {
		return preparedCommit{}, err
	}
	if options.MaxTransactionBytes > 0 && int64(len(line)+1) > options.MaxTransactionBytes {
		return preparedCommit{}, fmt.Errorf("%w: transaction is %d bytes, limit %d", ErrTooLarge, len(line)+1, options.MaxTransactionBytes)
	}
	prepared.Line = append(line, '\n')
	prepared.Envelope = envelope
	prepared.Result = CommitResult{
		TransactionID: txID,
		FirstSeq:      first,
		LastSeq:       envelope.LastSeq,
		Cursor:        envelope.LastSeq,
		Applied:       true,
	}
	return prepared, nil
}

func applyPrepared(state *logState, prepared preparedCommit) {
	if !prepared.Result.Applied {
		return
	}
	fingerprint := transactionFingerprint(prepared.Wires)
	state.Transactions[prepared.TransactionID] = storedTransaction{Result: prepared.Result, Fingerprint: fingerprint, InputFingerprint: prepared.InputFingerprint}
	for i, event := range prepared.Events {
		seq := prepared.Envelope.FirstSeq + Cursor(i)
		if prepared.Seqs != nil {
			seq = prepared.Seqs[i]
		}
		state.Records = append(state.Records, Record{Seq: seq, TransactionID: prepared.TransactionID, Event: event})
		if key := eventDedupeKey(event); key != "" {
			state.Dedupe[key] = dedupeEntry{Fingerprint: eventFingerprint(prepared.Wires[i]), Result: prepared.Result}
		}
	}
	state.Skipped = append(state.Skipped, prepared.Skipped...)
	state.Cursor = prepared.Result.Cursor
}

func eventFingerprint(wire wireEvent) string {
	data, _ := json.Marshal(wire)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func eventDedupeKey(event Event) string {
	switch e := event.(type) {
	case TurnStarted:
		return dedupeID(EventTypeTurnStarted, e.TurnID)
	case *TurnStarted:
		if e != nil {
			return dedupeID(EventTypeTurnStarted, e.TurnID)
		}
	case TurnFinished:
		return dedupeID(EventTypeTurnFinished, e.TurnID)
	case *TurnFinished:
		if e != nil {
			return dedupeID(EventTypeTurnFinished, e.TurnID)
		}
	case InputQueued:
		return dedupeID(EventTypeInputQueued, e.InputID)
	case *InputQueued:
		if e != nil {
			return dedupeID(EventTypeInputQueued, e.InputID)
		}
	case CommandScheduled:
		return dedupeID(EventTypeCommandScheduled, e.CommandID)
	case *CommandScheduled:
		if e != nil {
			return dedupeID(EventTypeCommandScheduled, e.CommandID)
		}
	case CommandCompleted:
		return dedupeID(EventTypeCommandCompleted, e.CommandID)
	case *CommandCompleted:
		if e != nil {
			return dedupeID(EventTypeCommandCompleted, e.CommandID)
		}
	case RequestPrepared:
		return dedupeID(EventTypeRequestPrepared, e.Manifest.RequestID)
	case *RequestPrepared:
		if e != nil {
			return dedupeID(EventTypeRequestPrepared, e.Manifest.RequestID)
		}
	case AttemptFinished:
		return dedupeID(EventTypeAttemptFinished, e.Attempt.AttemptID)
	case *AttemptFinished:
		if e != nil {
			return dedupeID(EventTypeAttemptFinished, e.Attempt.AttemptID)
		}
	case ToolStarted:
		return dedupeID(EventTypeToolStarted, e.TurnID+"/"+e.StepID+"/"+e.Call.CallID)
	case *ToolStarted:
		if e != nil {
			return dedupeID(EventTypeToolStarted, e.TurnID+"/"+e.StepID+"/"+e.Call.CallID)
		}
	case ToolFinished:
		return dedupeID(EventTypeToolFinished, e.TurnID+"/"+e.StepID+"/"+e.CallID)
	case *ToolFinished:
		if e != nil {
			return dedupeID(EventTypeToolFinished, e.TurnID+"/"+e.StepID+"/"+e.CallID)
		}
	case ApprovalRequested:
		return dedupeID(EventTypeApprovalRequested, e.Request.ApprovalID)
	case *ApprovalRequested:
		if e != nil {
			return dedupeID(EventTypeApprovalRequested, e.Request.ApprovalID)
		}
	case ApprovalResolved:
		return dedupeID(EventTypeApprovalResolved, e.Resolution.ApprovalID)
	case *ApprovalResolved:
		if e != nil {
			return dedupeID(EventTypeApprovalResolved, e.Resolution.ApprovalID)
		}
	case ToolResultsProjected:
		if e.StepID == "" {
			break
		}
		return dedupeID(EventTypeToolResultsProjected, e.TurnID+"/"+e.StepID)
	case *ToolResultsProjected:
		if e != nil && e.StepID != "" {
			return dedupeID(EventTypeToolResultsProjected, e.TurnID+"/"+e.StepID)
		}
	}
	return ""
}

func dedupeID(kind EventType, id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return string(kind) + ":" + id
}

var idCounter atomic.Uint64

func newID(prefix string) string {
	var bytesValue [16]byte
	if _, err := rand.Read(bytesValue[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(bytesValue[:])
	}
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), idCounter.Add(1))
}

func cloneEvent(event Event) (Event, error) {
	normalized, _, err := normalizeEvent(event)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func cloneRecords(records []Record) ([]Record, error) {
	result := make([]Record, 0, len(records))
	for _, record := range records {
		event, err := cloneEvent(record.Event)
		if err != nil {
			return nil, err
		}
		result = append(result, Record{Seq: record.Seq, TransactionID: record.TransactionID, Event: event})
	}
	return result, nil
}

func copyBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	return append([]byte(nil), data...)
}

func validateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	if strings.ContainsAny(id, "\x00\r\n") {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return nil
}

func sessionDir(root, sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if root == "." || root == "" {
		return "", fmt.Errorf("session root is required")
	}
	// root is the session root (for example ~/.ccdp/sessions), matching the
	// existing Config.SessionDir contract.  The caller that owns ~/.ccdp may
	// pass filepath.Join(dataRoot, "sessions") explicitly; guessing based on
	// a directory's basename would make arbitrary configured roots ambiguous.
	return filepath.Join(root, sessionID), nil
}

// SessionDir resolves a single session under an already-resolved session root.
func SessionDir(root, sessionID string) (string, error) { return sessionDir(root, sessionID) }

// ListSessions reports event-log session directories without creating or
// changing anything.  A missing root is an empty list.
func ListSessions(root string) ([]SessionInfo, error) {
	if strings.TrimSpace(root) == "" {
		return []SessionInfo{}, nil
	}
	root = filepath.Clean(root)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]SessionInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateSessionID(entry.Name()) != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "events.v1.jsonl")); err != nil {
			continue
		}
		info := SessionInfo{ID: entry.Name(), Dir: dir}
		if stat, err := entry.Info(); err == nil {
			info.ModifiedAt = stat.ModTime()
		}
		result = append(result, info)
	}
	return result, nil
}

type SessionInfo struct {
	ID         string
	Dir        string
	ModifiedAt time.Time
}

// ReadAll is a convenience for replay consumers and is intentionally built on
// Read, so it cannot accidentally use or mutate snapshot state.
func ReadAll(store ReadOnlyStore) ([]Record, error) {
	if store == nil {
		return nil, errors.New("session: nil store")
	}
	return store.Read(Beginning)
}

func syncFile(f *os.File, fn atomicfile.SyncFile) error {
	if fn != nil {
		return fn(f)
	}
	return f.Sync()
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func isUnexpectedEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return strings.Contains(err.Error(), "unexpected end of JSON input")
}
