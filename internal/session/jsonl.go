package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccdp/internal/atomicfile"
)

const (
	eventsFileName   = "events.v1.jsonl"
	writerLockName   = "writer.lock"
	snapshotFileName = "snapshot.json"
)

// JSONLStore is a single-writer append-only session log.  A writer handle
// owns writer.lock for its entire lifetime; read-only handles never acquire
// the lock and never repair files.
type JSONLStore struct {
	mu           sync.Mutex
	dir          string
	sessionID    string
	eventsPath   string
	lock         *atomicfile.Lock
	events       *os.File
	readOnly     bool
	closed       bool
	persistErr   error
	state        logState
	options      JSONLOptions
	recovery     RecoveryReport
	needsNewline bool
}

type logTail struct {
	offset int64
	bytes  int64
}

// OpenJSONLStore opens a writable store under root, where root is the
// already-resolved sessions directory (for example ~/.ccdp/sessions).
func OpenJSONLStore(root, sessionID string, options ...JSONLOption) (*JSONLStore, error) {
	dir, err := sessionDir(root, sessionID)
	if err != nil {
		return nil, err
	}
	return OpenJSONLStoreAt(dir, options...)
}

// OpenJSONLStoreAt opens a writable store at an already-resolved
// sessions/<id> directory.
func OpenJSONLStoreAt(dir string, options ...JSONLOption) (*JSONLStore, error) {
	return openJSONL(dir, false, options...)
}

// OpenJSONLReadOnly opens a replay/listing handle.  It performs no mkdir,
// truncate, lock acquisition, import, or other file mutation.
func OpenJSONLReadOnly(root, sessionID string, options ...JSONLOption) (*JSONLStore, error) {
	dir, err := sessionDir(root, sessionID)
	if err != nil {
		return nil, err
	}
	return OpenJSONLReadOnlyAt(dir, options...)
}

// OpenJSONLReadOnlyAt opens a read-only handle at an exact session directory.
func OpenJSONLReadOnlyAt(dir string, options ...JSONLOption) (*JSONLStore, error) {
	return openJSONL(dir, true, options...)
}

func openJSONL(dir string, readOnly bool, options ...JSONLOption) (*JSONLStore, error) {
	dir = filepath.Clean(dir)
	if dir == "." || dir == "" || filepath.Base(dir) == "." || filepath.Base(dir) == ".." {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSessionID, filepath.Base(dir))
	}
	sessionID := filepath.Base(dir)
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	opts := applyJSONLOptions(options)

	if readOnly {
		state, noNewline, _, err := readLog(filepath.Join(dir, eventsFileName), opts.MaxTransactionBytes)
		if err != nil {
			return nil, err
		}
		return &JSONLStore{
			dir: dir, sessionID: sessionID,
			eventsPath: filepath.Join(dir, eventsFileName),
			readOnly:   true, state: state, options: opts,
			needsNewline: noNewline,
		}, nil
	}

	// Remember the nearest pre-existing directory.  Once the session is
	// published, fsync every directory MkdirAll may have created, up through
	// that ancestor, so a crash cannot lose the new sessions/<id> entry.
	existingAncestor := nearestExistingDirectory(filepath.Dir(dir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create session directory %s: %w", dir, err)
	}
	lock, err := atomicfile.AcquireLock(filepath.Join(dir, writerLockName))
	if err != nil {
		if errors.Is(err, atomicfile.ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrWriterLocked, dir)
		}
		return nil, err
	}
	release := true
	defer func() {
		if release {
			_ = lock.Close()
		}
	}()

	eventsPath := filepath.Join(dir, eventsFileName)
	if _, err := os.Stat(eventsPath); errors.Is(err, os.ErrNotExist) {
		f, createErr := os.OpenFile(eventsPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, fmt.Errorf("session: create event log %s: %w", eventsPath, createErr)
		}
		if syncErr := syncFile(f, opts.SyncFile); syncErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("session: sync new event log %s: %w", eventsPath, syncErr)
		}
		closeErr := f.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("session: close new event log %s: %w", eventsPath, closeErr)
		}
		if err := syncDir(filepath.Dir(eventsPath), opts.SyncDir); err != nil {
			return nil, fmt.Errorf("session: sync session directory %s: %w", dir, err)
		}
		if err := syncCreatedDirectories(dir, existingAncestor, opts.SyncDir); err != nil {
			return nil, fmt.Errorf("session: sync session directory chain %s: %w", dir, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("session: stat event log %s: %w", eventsPath, err)
	}

	state, noNewline, tail, err := readLog(eventsPath, opts.MaxTransactionBytes)
	if err != nil {
		if errors.Is(err, ErrTorn) && tail != nil {
			reportPath, reportBytes, recoveryErr := recoverTail(eventsPath, tail.offset, tail.bytes, opts)
			if recoveryErr != nil {
				return nil, fmt.Errorf("session: recover torn event log %s: %w", eventsPath, recoveryErr)
			}
			state, noNewline, _, err = readLog(eventsPath, opts.MaxTransactionBytes)
			if err != nil {
				return nil, fmt.Errorf("session: validate recovered event log %s: %w", eventsPath, err)
			}
			_ = reportBytes
			store := &JSONLStore{
				dir: dir, sessionID: sessionID, eventsPath: eventsPath, lock: lock,
				readOnly: false, state: state, options: opts,
				recovery:     RecoveryReport{Recovered: true, TailPath: reportPath, TailBytes: tail.bytes},
				needsNewline: noNewline,
			}
			if err := store.openAppend(); err != nil {
				return nil, err
			}
			release = false
			return store, nil
		}
		return nil, fmt.Errorf("session: open event log %s: %w", eventsPath, err)
	}
	store := &JSONLStore{
		dir: dir, sessionID: sessionID, eventsPath: eventsPath, lock: lock,
		readOnly: false, state: state, options: opts, needsNewline: noNewline,
	}
	if err := store.openAppend(); err != nil {
		return nil, err
	}
	release = false
	return store, nil
}

func nearestExistingDirectory(path string) string {
	path = filepath.Clean(path)
	for {
		if stat, err := os.Stat(path); err == nil && stat.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func syncCreatedDirectories(dir, stop string, syncFn func(string) error) error {
	dir = filepath.Clean(dir)
	stop = filepath.Clean(stop)
	for {
		if err := syncDir(dir, syncFn); err != nil {
			return err
		}
		if dir == stop {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func (s *JSONLStore) openAppend() error {
	f, err := os.OpenFile(s.eventsPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open event log for append %s: %w", s.eventsPath, err)
	}
	s.events = f
	if s.needsNewline {
		if err := writeAll(f, []byte{'\n'}); err != nil {
			_ = f.Close()
			s.events = nil
			return fmt.Errorf("session: finish event-log line %s: %w", s.eventsPath, err)
		}
		if err := syncFile(f, s.options.SyncFile); err != nil {
			_ = f.Close()
			s.events = nil
			return fmt.Errorf("session: sync event-log separator %s: %w", s.eventsPath, err)
		}
		s.needsNewline = false
	}
	return nil
}

func (s *JSONLStore) Commit(expected Cursor, batch Batch) (CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CommitResult{}, ErrClosed
	}
	if s.readOnly {
		return CommitResult{}, ErrReadOnly
	}
	if s.persistErr != nil {
		return CommitResult{}, fmt.Errorf("%w: %v", ErrPersistenceFailed, s.persistErr)
	}
	prepared, err := prepareCommit(&s.state, expected, batch, s.options)
	if err != nil {
		return CommitResult{}, err
	}
	if !prepared.Result.Applied {
		return prepared.Result, nil
	}
	if s.needsNewline {
		if err := writeAll(s.events, []byte{'\n'}); err != nil {
			return CommitResult{}, s.failPersistence(err)
		}
		s.needsNewline = false
	}
	if err := writeAll(s.events, prepared.Line); err != nil {
		return CommitResult{}, s.failPersistence(err)
	}
	if err := syncFile(s.events, s.options.SyncFile); err != nil {
		return CommitResult{}, s.failPersistence(err)
	}
	applyPrepared(&s.state, prepared)
	return prepared.Result, nil
}

func (s *JSONLStore) failPersistence(err error) error {
	if err == nil {
		err = errors.New("unknown persistence failure")
	}
	s.persistErr = err
	return fmt.Errorf("%w: %v", ErrPersistenceFailed, err)
}

func (s *JSONLStore) Read(after Cursor) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var records []Record
	for _, record := range s.state.Records {
		if record.Seq > after {
			records = append(records, record)
		}
	}
	return cloneRecords(records)
}

func (s *JSONLStore) CurrentCursor() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Cursor
}

func (s *JSONLStore) PersistenceEnabled() bool { return true }

func (s *JSONLStore) RecoveryReport() RecoveryReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovery
}

func (s *JSONLStore) SessionID() string  { return s.sessionID }
func (s *JSONLStore) Dir() string        { return s.dir }
func (s *JSONLStore) EventsPath() string { return s.eventsPath }

type snapshotDisk struct {
	SchemaVersion uint32    `json:"schema_version"`
	SessionID     string    `json:"session_id"`
	LastSeq       Cursor    `json:"last_seq"`
	UpdatedAt     time.Time `json:"updated_at"`
	Data          []byte    `json:"data,omitempty"`
}

func (s *JSONLStore) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, ErrClosed
	}
	data, err := os.ReadFile(filepath.Join(s.dir, snapshotFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("session: read snapshot: %w", err)
	}
	var disk snapshotDisk
	if err := decodeStrict(data, &disk); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrSnapshotInvalid, err)
	}
	if disk.SchemaVersion > SchemaVersion {
		return Snapshot{}, fmt.Errorf("%w: snapshot schema_version %d", ErrFutureVersion, disk.SchemaVersion)
	}
	if disk.SchemaVersion != SchemaVersion || disk.SessionID != s.sessionID || disk.UpdatedAt.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: snapshot metadata", ErrSnapshotInvalid)
	}
	if disk.LastSeq > s.state.Cursor {
		return Snapshot{}, fmt.Errorf("%w: last_seq %d exceeds log cursor %d", ErrSnapshotInvalid, disk.LastSeq, s.state.Cursor)
	}
	return Snapshot{SchemaVersion: disk.SchemaVersion, SessionID: disk.SessionID, LastSeq: disk.LastSeq, UpdatedAt: disk.UpdatedAt, Data: copyBytes(disk.Data)}, nil
}

func (s *JSONLStore) SaveSnapshot(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.readOnly {
		return ErrReadOnly
	}
	if snapshot.SchemaVersion == 0 {
		snapshot.SchemaVersion = SchemaVersion
	}
	if snapshot.SchemaVersion > SchemaVersion {
		return ErrFutureVersion
	}
	if snapshot.SchemaVersion != SchemaVersion {
		return ErrSnapshotInvalid
	}
	if snapshot.SessionID == "" {
		snapshot.SessionID = s.sessionID
	}
	if snapshot.SessionID != s.sessionID {
		return fmt.Errorf("%w: snapshot session ID %q", ErrSnapshotInvalid, snapshot.SessionID)
	}
	if snapshot.LastSeq > s.state.Cursor {
		return &CursorConflictError{Expected: snapshot.LastSeq, Actual: s.state.Cursor}
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now().UTC()
	}
	disk := snapshotDisk{SchemaVersion: snapshot.SchemaVersion, SessionID: snapshot.SessionID, LastSeq: snapshot.LastSeq, UpdatedAt: snapshot.UpdatedAt, Data: copyBytes(snapshot.Data)}
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if s.options.MaxTransactionBytes > 0 && int64(len(data)) > s.options.MaxTransactionBytes {
		return fmt.Errorf("%w: snapshot is %d bytes, limit %d", ErrTooLarge, len(data), s.options.MaxTransactionBytes)
	}
	if err := atomicfile.WriteFileWithSync(filepath.Join(s.dir, snapshotFileName), data, 0o600, s.options.SyncFile, s.options.SyncDir); err != nil {
		return err
	}
	return nil
}

func (s *JSONLStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	events := s.events
	lock := s.lock
	s.events = nil
	s.lock = nil
	s.mu.Unlock()
	var result error
	if events != nil {
		if err := syncFile(events, s.options.SyncFile); err != nil {
			result = err
		}
		if err := events.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if lock != nil {
		if err := lock.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// readLog validates every complete transaction without changing path.  The
// returned tail is only a recovery candidate; callers must hold writer.lock
// before truncating it.
func readLog(path string, maxBytes int64) (logState, bool, *logTail, error) {
	state := newLogState()
	f, err := os.Open(path)
	if err != nil {
		return state, false, nil, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	noNewline := false
	for {
		line, terminated, readErr := readLineLimited(reader, maxBytes)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			// A bounded-reader admission error is a caller/configuration
			// problem, not evidence that the existing log is corrupt. Preserve
			// ErrTooLarge so callers can reject/retry without triggering
			// destructive recovery or a persistence-failure latch.
			if errors.Is(readErr, ErrTooLarge) {
				return state, false, nil, fmt.Errorf("%w: read %s at byte %d: %v", ErrTooLarge, path, offset, readErr)
			}
			return state, false, nil, fmt.Errorf("%w: read %s at byte %d: %v", ErrCorrupt, path, offset, readErr)
		}
		lineOffset := offset
		offset += int64(len(line))
		if len(line) == 0 {
			return state, false, nil, fmt.Errorf("%w: empty transaction line at byte %d", ErrCorrupt, lineOffset)
		}
		data := line
		if terminated {
			data = data[:len(data)-1]
			if len(data) > 0 && data[len(data)-1] == '\r' {
				data = data[:len(data)-1]
			}
		}
		envelope, events, wires, parseErr := parseTransaction(data)
		if parseErr != nil {
			if !terminated && errors.Is(parseErr, ErrTorn) {
				return state, false, &logTail{offset: lineOffset, bytes: int64(len(line))}, fmt.Errorf("%w", parseErr)
			}
			return state, false, nil, fmt.Errorf("%s line at byte %d: %w", path, lineOffset, parseErr)
		}
		if envelope.FirstSeq != state.Cursor+1 {
			return state, false, nil, fmt.Errorf("%w: sequence starts at %d, expected %d", ErrCorrupt, envelope.FirstSeq, state.Cursor+1)
		}
		if _, exists := state.Transactions[envelope.TransactionID]; exists {
			return state, false, nil, fmt.Errorf("%w: duplicate transaction %q", ErrCorrupt, envelope.TransactionID)
		}
		for i, event := range events {
			if key := eventDedupeKey(event); key != "" {
				if prior, exists := state.Dedupe[key]; exists {
					if prior.Fingerprint != eventFingerprint(wires[i]) {
						return state, false, nil, fmt.Errorf("%w: duplicate ID %s with changed content", ErrCorrupt, key)
					}
					return state, false, nil, fmt.Errorf("%w: duplicate ID %s", ErrCorrupt, key)
				}
			}
		}
		result := CommitResult{TransactionID: envelope.TransactionID, FirstSeq: envelope.FirstSeq, LastSeq: envelope.LastSeq, Cursor: envelope.LastSeq, Applied: true}
		applyPrepared(&state, preparedCommit{TransactionID: envelope.TransactionID, InputFingerprint: envelope.InputFingerprint, Events: events, Wires: wires, Envelope: envelope, Result: result})
		if !terminated {
			noNewline = true
		}
	}
	return state, noNewline, nil, nil
}

func readLineLimited(reader *bufio.Reader, maxBytes int64) ([]byte, bool, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTransactionBytes
	}
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if int64(len(line)+len(part)) > maxBytes {
			return nil, false, fmt.Errorf("%w: line exceeds %d bytes", ErrTooLarge, maxBytes)
		}
		line = append(line, part...)
		if err == nil {
			return line, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, false, io.EOF
			}
			return line, false, nil
		}
		return nil, false, err
	}
}

func recoverTail(path string, offset, bytesCount int64, options JSONLOptions) (string, int64, error) {
	if bytesCount <= 0 {
		return "", 0, fmt.Errorf("%w: empty torn tail", ErrTorn)
	}
	src, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	tailPath := fmt.Sprintf("%s.corrupt.%d", path, time.Now().UnixNano())
	_, err = atomicfile.WriteReaderWithSync(tailPath, io.LimitReader(src, bytesCount), 0o600, bytesCount, options.SyncFile, options.SyncDir)
	if err != nil {
		return "", 0, err
	}
	mut, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", 0, err
	}
	if err := mut.Truncate(offset); err != nil {
		_ = mut.Close()
		return "", 0, err
	}
	if err := syncFile(mut, options.SyncFile); err != nil {
		_ = mut.Close()
		return "", 0, err
	}
	if err := mut.Close(); err != nil {
		return "", 0, err
	}
	if err := syncDir(filepath.Dir(path), options.SyncDir); err != nil {
		return "", 0, err
	}
	return tailPath, bytesCount, nil
}

func syncDir(dir string, fn func(string) error) error {
	if fn != nil {
		return fn(dir)
	}
	return atomicfile.SyncDir(dir)
}

// Compile-time checks keep the two implementations on the same contract.
var _ Store = (*JSONLStore)(nil)

// Ensure imports remain intentional when building with a platform-specific
// atomicfile lock implementation.
var _ = strings.Builder{}
