package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testInput(id, text string) InputQueued {
	return InputQueued{InputID: id, Text: text, Strategy: "steer"}
}

func testMessage(id, text string) Message {
	return Message{MessageID: id, Role: "user", Content: []ContentBlock{{Kind: ContentText, Text: text}}}
}

func TestTypedEventsRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	st, err := OpenJSONLStore(root, "typed")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	args := json.RawMessage(`{"path":"README.md"}`)
	events := []Event{
		SessionCreated{SessionID: "typed", FormatVersion: 1, CreatedAt: now},
		SettingsChanged{Revision: 1, Settings: Settings{Model: "model", Provider: "provider", ExecutionMode: "execute"}},
		TurnStarted{TurnID: "turn", StepID: "step"},
		RequestPrepared{Manifest: RequestManifest{RequestID: "request", Model: "model", Provider: "provider", AdapterVersion: "v1", Messages: []Message{testMessage("m1", "hello")}, Digest: "digest"}},
		AssistantCommitted{TurnID: "turn", Message: testMessage("m2", "answer"), ToolCalls: []ToolCall{{CallID: "call", ToolID: "read", Arguments: args}}},
		ToolStarted{TurnID: "turn", Call: ToolCall{CallID: "call", ToolID: "read", Arguments: args}, Fingerprint: "fingerprint"},
		ToolFinished{TurnID: "turn", CallID: "call", Status: "success", Result: ToolResult{CallID: "call", Status: "success", Text: "ok"}},
		ApprovalRequested{Request: ApprovalRequest{ApprovalID: "approval", SessionID: "typed", ArgumentDigest: "digest", Workspace: "."}},
		ApprovalResolved{Resolution: ApprovalResolution{ApprovalID: "approval", Decision: "allow"}},
		TurnFinished{TurnID: "turn", Outcome: "success"},
	}
	if _, err := st.Commit(0, Batch{Events: events}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJSONLStore(root, "typed")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records, err := reopened.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(events) {
		t.Fatalf("round-trip event count = %d, want %d", len(records), len(events))
	}
	if _, ok := records[3].Event.(*RequestPrepared); !ok {
		t.Fatalf("manifest event type = %T", records[3].Event)
	}
	if _, ok := records[6].Event.(*ToolFinished); !ok {
		t.Fatalf("tool result event type = %T", records[6].Event)
	}
}

func TestStoreConformanceMemoryAndJSONL(t *testing.T) {
	tests := []struct {
		name string
		open func(t *testing.T) Store
	}{
		{name: "memory", open: func(*testing.T) Store { return NewMemoryStore() }},
		{name: "jsonl", open: func(t *testing.T) Store {
			store, err := OpenJSONLStore(filepath.Join(t.TempDir(), "sessions"), "conformance")
			if err != nil {
				t.Fatal(err)
			}
			return store
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			defer store.Close()
			first, err := store.Commit(0, Batch{TransactionID: "tx-1", Events: []Event{testInput("input-1", "hello")}})
			if err != nil {
				t.Fatal(err)
			}
			if !first.Applied || first.FirstSeq != 1 || first.LastSeq != 1 || first.Cursor != 1 {
				t.Fatalf("unexpected first result: %+v", first)
			}
			if _, err := store.Commit(0, Batch{Events: []Event{testInput("input-2", "stale")}}); !errors.Is(err, ErrCursorConflict) {
				t.Fatalf("expected cursor conflict, got %v", err)
			}

			retry, err := store.Commit(0, Batch{TransactionID: "tx-1", Events: []Event{testInput("input-1", "hello")}})
			if err != nil {
				t.Fatal(err)
			}
			if !retry.Deduplicated || retry.Applied || retry.Cursor != first.Cursor {
				t.Fatalf("expected idempotent retry, got %+v", retry)
			}
			if _, err := store.Commit(1, Batch{Events: []Event{testInput("input-1", "changed")}}); !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("expected duplicate ID rejection, got %v", err)
			}

			second, err := store.Commit(first.Cursor, Batch{Events: []Event{testInput("input-2", "world")}})
			if err != nil {
				t.Fatal(err)
			}
			if second.Cursor != 2 {
				t.Fatalf("expected cursor 2, got %+v", second)
			}
			records, err := store.Read(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 || records[0].Seq != 1 || records[1].Seq != 2 {
				t.Fatalf("unexpected records: %+v", records)
			}
			if got, ok := records[0].Event.(*InputQueued); !ok || got.Text != "hello" {
				t.Fatalf("unexpected decoded event %#v", records[0].Event)
			}

			snapshot := Snapshot{LastSeq: 2, Data: []byte("projection")}
			if err := store.SaveSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			gotSnapshot, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if gotSnapshot.LastSeq != 2 || string(gotSnapshot.Data) != "projection" {
				t.Fatalf("unexpected snapshot %+v", gotSnapshot)
			}
		})
	}
}

func TestJSONLReopensAndRecoversTornTail(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := OpenJSONLStore(root, "resume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(0, Batch{Events: []Event{testInput("one", "one")}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "resume", eventsFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(before, []byte(`{"schema_version":1,"transaction_id":"torn"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenJSONLReadOnly(root, "resume")
	if !errors.Is(err, ErrTorn) || readOnly != nil {
		t.Fatalf("read-only open must reject torn tail without repair, store=%v err=%v", readOnly, err)
	}
	afterReadOnly, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterReadOnly) != string(append(before, []byte(`{"schema_version":1,"transaction_id":"torn"`)...)) {
		t.Fatal("read-only open changed event log")
	}

	reopened, err := OpenJSONLStore(root, "resume")
	if err != nil {
		t.Fatal(err)
	}
	report := reopened.RecoveryReport()
	if !report.Recovered || report.TailPath == "" || report.TailBytes == 0 {
		t.Fatalf("missing recovery report: %+v", report)
	}
	if _, err := os.Stat(report.TailPath); err != nil {
		t.Fatalf("missing preserved tail: %v", err)
	}
	if got := reopened.CurrentCursor(); got != 1 {
		t.Fatalf("recovery changed cursor: %d", got)
	}
	if _, err := reopened.Commit(1, Batch{Events: []Event{testInput("two", "two")}}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLRejectsCompleteCorruptionAndFutureVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := OpenJSONLStore(root, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(0, Batch{Events: []Event{testInput("one", "one")}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "corrupt", eventsFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte("{not-json}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLStore(root, "corrupt"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected complete line corruption, got %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(append(content, []byte("{not-json}\n")...)) {
		t.Fatal("complete corruption was silently truncated")
	}

	futureRoot := filepath.Join(t.TempDir(), "sessions")
	future, err := OpenJSONLStore(futureRoot, "future")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Commit(0, Batch{Events: []Event{testInput("one", "one")}}); err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	futurePath := filepath.Join(futureRoot, "future", eventsFileName)
	futureData, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatal(err)
	}
	futureData = []byte(strings.Replace(string(futureData), `"schema_version":1`, `"schema_version":99`, 1))
	if err := os.WriteFile(futurePath, futureData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLStore(futureRoot, "future"); !errors.Is(err, ErrFutureVersion) {
		t.Fatalf("expected future version rejection, got %v", err)
	}
}

func TestJSONLTooLargeLinePreservesAdmissionError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	dir := filepath.Join(root, "too-large")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := append([]byte(`{"schema_version":1,"transaction_id":"oversized","events":[`),
		[]byte(strings.Repeat("x", int(DefaultMaxTransactionBytes)))...)
	if err := os.WriteFile(filepath.Join(dir, eventsFileName), line, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenJSONLReadOnly(root, "too-large")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized JSONL line error=%v, want ErrTooLarge", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized JSONL line was misclassified as corruption: %v", err)
	}
}

func TestJSONLRecoversEveryTruncatedByteOfFinalTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	seed, err := OpenJSONLStore(root, "truncate-seed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(0, Batch{Events: []Event{testInput("one", "payload")}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "truncate-seed", eventsFileName)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 1; cut < len(full); cut++ {
		cut := cut
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "sessions")
			if err := os.MkdirAll(filepath.Join(dir, "s"), 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, "s", eventsFileName)
			if err := os.WriteFile(p, full[:cut], 0o600); err != nil {
				t.Fatal(err)
			}
			st, openErr := OpenJSONLStore(dir, "s")
			if openErr != nil {
				t.Fatal(openErr)
			}
			if cut == len(full)-1 {
				// Removing only the final newline leaves a complete JSON value;
				// the writer adds a separator before its next append.
			} else {
				report := st.RecoveryReport()
				if !report.Recovered {
					t.Fatalf("cut %d was not reported as recovered", cut)
				}
				if report.TailBytes != int64(cut) {
					t.Fatalf("tail bytes = %d, want %d", report.TailBytes, cut)
				}
			}
			wantCursor := Cursor(0)
			if cut == len(full)-1 {
				wantCursor = 1
			}
			if st.CurrentCursor() != wantCursor {
				t.Fatalf("truncated transaction became durable at cut %d", cut)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMixedBatchDedupeRetryPreservesOriginalIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	st, err := OpenJSONLStore(root, "mixed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(0, Batch{Events: []Event{testInput("already", "old")}}); err != nil {
		t.Fatal(err)
	}
	batch := Batch{TransactionID: "mixed-tx", Events: []Event{
		testInput("already", "old"),
		CommandCompleted{CommandID: "command-1", Outcome: "success", Report: "done"},
	}}
	first, err := st.Commit(1, batch)
	if err != nil {
		t.Fatal(err)
	}
	if first.FirstSeq != 2 || first.LastSeq != 2 {
		t.Fatalf("filtered batch result = %+v", first)
	}
	retry, err := st.Commit(0, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Deduplicated || retry.Applied || retry.Cursor != first.Cursor {
		t.Fatalf("mixed retry = %+v", retry)
	}
	records, err := st.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("mixed retry appended records: %+v", records)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJSONLStore(root, "mixed")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retryAfterReopen, err := reopened.Commit(0, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !retryAfterReopen.Deduplicated || retryAfterReopen.Applied || retryAfterReopen.Cursor != first.Cursor {
		t.Fatalf("mixed retry after reopen = %+v", retryAfterReopen)
	}
}

func TestReadResultsAreMutationIsolated(t *testing.T) {
	st := NewMemoryStore()
	if _, err := st.Commit(0, Batch{Events: []Event{testInput("one", "original")}}); err != nil {
		t.Fatal(err)
	}
	first, err := st.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Event.(*InputQueued).Text = "mutated"
	second, err := st.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := second[0].Event.(*InputQueued).Text; got != "original" {
		t.Fatalf("read mutation leaked into store: %q", got)
	}
}

func TestJSONLWriterLockAcrossProcess(t *testing.T) {
	if os.Getenv("CCDP_SESSION_LOCK_HELPER") == "1" {
		root := os.Getenv("CCDP_SESSION_LOCK_ROOT")
		st, err := OpenJSONLStore(root, "cross-process")
		if err != nil {
			fmt.Fprintln(os.Stdout, "ERROR", err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "READY")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = st.Close()
		return
	}
	root := filepath.Join(t.TempDir(), "sessions")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLWriterLockAcrossProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(), "CCDP_SESSION_LOCK_HELPER=1", "CCDP_SESSION_LOCK_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "READY" {
		t.Fatalf("lock helper output %q", line)
	}
	if _, err := OpenJSONLStore(root, "cross-process"); !errors.Is(err, ErrWriterLocked) {
		t.Fatalf("expected cross-process lock error, got %v", err)
	}
	_, _ = stdin.Write([]byte("close\n"))
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLWriterLockAndPersistenceFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	first, err := OpenJSONLStore(root, "locked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLStore(root, "locked"); !errors.Is(err, ErrWriterLocked) {
		t.Fatalf("expected writer lock error, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var syncCalls atomic.Int32
	second, err := OpenJSONLStore(root, "failed", WithSyncFile(func(*os.File) error {
		if syncCalls.Add(1) > 1 {
			return errors.New("injected sync failure")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Commit(0, Batch{Events: []Event{testInput("one", "one")}}); !errors.Is(err, ErrPersistenceFailed) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	if _, err := second.Commit(0, Batch{Events: []Event{testInput("two", "two")}}); !errors.Is(err, ErrPersistenceFailed) {
		t.Fatalf("failed store accepted another write: %v", err)
	}
	if err := second.Close(); err == nil {
		t.Fatal("expected close to report injected sync failure")
	}
}

func TestReadOnlyListingDoesNotCreateOrRepair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	listed, err := ListSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("unexpected sessions: %+v", listed)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created root: %v", err)
	}
}

func TestArtifactStorePublishesImmutableContent(t *testing.T) {
	dir := t.TempDir()
	artifacts, err := NewArtifactStore(dir, WithMaxBlobBytes(64))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Size != 5 || len(ref.Hash) != 64 {
		t.Fatalf("unexpected ref %+v", ref)
	}
	data, err := artifacts.Read(ref, 0)
	if err != nil || string(data) != "hello" {
		t.Fatalf("artifact read = %q, err=%v", data, err)
	}
	if err := artifacts.Verify(ref); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifacts.Dir(), ref.Hash)
	if err := os.WriteFile(path, []byte("jello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Verify(ref); err == nil {
		t.Fatal("same-size blob corruption was not detected")
	}
	if _, err := artifacts.Read(ref, 0); err == nil {
		t.Fatal("same-size blob corruption was returned by Read")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Open(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing blob error = %v", err)
	}
	ref, err = artifacts.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Put([]byte(strings.Repeat("x", 65))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected blob size error, got %v", err)
	}
	readOnly := OpenArtifactStoreReadOnly(dir)
	if _, err := readOnly.Put([]byte("no")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected read-only blob error, got %v", err)
	}
}
