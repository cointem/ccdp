package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestInputAdmissionSubmitCancellationAfterEnqueueIsUnknown(t *testing.T) {
	for _, cancelRoot := range []bool{true, false} {
		name := "caller"
		if cancelRoot {
			name = "runtime"
		}
		t.Run(name, func(t *testing.T) {
			rootCtx, rootCancel := context.WithCancel(context.Background())
			defer rootCancel()
			callerCtx, callerCancel := context.WithCancel(context.Background())
			defer callerCancel()
			// Hold the owner loop at the queue boundary: receipt production is
			// deliberately blocked, so cancellation happens after admission.
			a := &Agent{
				conversation: conversationState{sessionID: "admission-test"},
				lifecycle:    sessionLifecycle{rootCtx: rootCtx, runStartedFlag: true},
				owner:        &sessionOwner{commands: make(chan runtimeCommand, 1)},
				inbox: dedupState{
					seenCommands: make(map[protocol.CommandID]string),
					seenReceipts: make(map[protocol.CommandID]protocol.Receipt),
				},
			}
			cmd := protocol.NewSubmitInput("admission-command", "admission-test", "admission-input", "hello", protocol.InputFollowup)
			type outcome struct {
				receipt protocol.Receipt
				err     error
			}
			result := make(chan outcome, 1)
			go func() {
				r, err := a.Submit(callerCtx, cmd)
				result <- outcome{r, err}
			}()
			select {
			case request := <-a.owner.commands:
				if !reflect.DeepEqual(request.command, cmd) {
					t.Fatalf("queued command changed: %+v", request.command)
				}
			case <-time.After(time.Second):
				t.Fatal("Submit did not enqueue")
			}
			if cancelRoot {
				rootCancel()
			} else {
				callerCancel()
			}
			select {
			case got := <-result:
				if !errors.Is(got.err, context.Canceled) || !reflect.DeepEqual(got.receipt, protocol.Receipt{}) {
					t.Fatalf("post-enqueue cancellation = %+v, %v; want empty receipt and transport cancellation", got.receipt, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Submit did not return after cancellation")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.inbox.seenReceipts) != 0 || len(a.inbox.seenCommands) != 0 {
				t.Fatalf("unknown admission was cached as a definite result: %+v", a.inbox.seenReceipts)
			}
		})
	}
}

// admissionBoundaryStore fails at a specific durable boundary without starting
// a provider request. Commit is invoked while sessionPersistence.mu is held.
type admissionBoundaryStore struct {
	session.Store
	failCommit     session.EventType
	afterCommit    func(session.EventType)
	failNextRead   bool
	readFailures   int
	committedQueue int
}

func (s *admissionBoundaryStore) Commit(cursor session.Cursor, batch session.Batch) (session.CommitResult, error) {
	for _, event := range batch.Events {
		if event.Type() == s.failCommit {
			return session.CommitResult{}, session.ErrPersistenceFailed
		}
	}
	result, err := s.Store.Commit(cursor, batch)
	if err == nil && result.Applied {
		for _, event := range batch.Events {
			if event.Type() == session.EventTypeInputQueued {
				s.committedQueue++
			}
			if s.afterCommit != nil {
				s.afterCommit(event.Type())
			}
		}
	}
	return result, err
}

func (s *admissionBoundaryStore) Read(after session.Cursor) ([]session.Record, error) {
	if s.failNextRead {
		s.failNextRead = false
		s.readFailures++
		return nil, session.ErrPersistenceFailed
	}
	return s.Store.Read(after)
}

func TestInputAdmissionDurableInputKeepsScheduledReceipt(t *testing.T) {
	for _, boundary := range []string{"closing", "delivery-write", "history-read"} {
		t.Run(boundary, func(t *testing.T) {
			cfg := testPersistenceConfig(t)
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			p := a.persistenceHandle()
			backend := journalBackend(t, p)
			backend.mu.Lock()
			store := &admissionBoundaryStore{Store: backend.store}
			switch boundary {
			case "closing":
				store.afterCommit = func(kind session.EventType) {
					if kind == session.EventTypeInputQueued {
						a.mu.Lock()
						a.lifecycle.closing = true
						a.mu.Unlock()
					}
				}
			case "delivery-write":
				store.failCommit = session.EventTypeInputDelivered
			case "history-read":
				store.afterCommit = func(kind session.EventType) {
					if kind == session.EventTypeInputDelivered {
						// Report a changed backend cursor so the next projection
						// must refresh through the injected failing reader.
						store.failNextRead = true
					}
				}
			}
			backend.store = store
			backend.mu.Unlock()
			cmd := protocol.NewSubmitInput("durable-command", protocol.SessionID(a.SessionID()), "durable-input", "accepted exactly once", protocol.InputFollowup)
			receipt := a.applyCommand(cmd)
			if receipt.Status != protocol.ReceiptScheduled || receipt.Error != nil || receipt.OperationID != protocol.OperationID(cmd.ID) {
				t.Fatalf("durably accepted input receipt = %+v; want Scheduled", receipt)
			}
			if retry := a.applyCommand(cmd); !reflect.DeepEqual(retry, receipt) {
				t.Fatalf("same-command retry = %+v; want original %+v", retry, receipt)
			}
			if store.committedQueue != 1 {
				t.Fatalf("durable input committed %d times; want one", store.committedQueue)
			}
			a.mu.Lock()
			cached, cachedBody, busy := a.inbox.seenInputs[cmd.Input.ID], a.inbox.seenInputBody[cmd.Input.ID], a.turn.busy
			a.mu.Unlock()
			if !reflect.DeepEqual(cached, receipt) || cachedBody == "" || busy {
				t.Fatalf("admission identity/receipt lost or provider started: cached=%+v digest=%q busy=%v", cached, cachedBody, busy)
			}
			if boundary == "history-read" && store.readFailures != 1 {
				t.Fatalf("history append failure was not exercised: reads=%d", store.readFailures)
			}
			if boundary != "closing" && !errors.Is(a.persistenceFailure(), session.ErrPersistenceFailed) {
				t.Fatalf("storage failure was not latched: %v", a.persistenceFailure())
			}
			// Read the authoritative log directly, bypassing the injected error:
			// recovery must retain exactly one pending or delivered user input.
			snapshot, err := SessionSnapshotProjection(store.Store, a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "history-read" {
				if len(snapshot.History) != 1 || snapshot.History[0].Content != cmd.Input.Text || len(snapshot.PendingInputs) != 0 {
					t.Fatalf("delivered input lost on replay: %+v", snapshot)
				}
			} else if len(snapshot.PendingInputs) != 1 || snapshot.PendingInputs[0].ID != cmd.Input.ID || len(snapshot.History) != 0 {
				t.Fatalf("accepted pending input lost on replay: %+v", snapshot)
			}
		})
	}
}

func TestInputAdmissionFailedQueueCommitRemainsRejected(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	p := a.persistenceHandle()
	backend := journalBackend(t, p)
	backend.mu.Lock()
	store := &admissionBoundaryStore{Store: backend.store, failCommit: session.EventTypeInputQueued}
	backend.store = store
	backend.mu.Unlock()
	cmd := protocol.NewSubmitInput("unaccepted-command", protocol.SessionID(a.SessionID()), "unaccepted-input", "not accepted", protocol.InputFollowup)
	receipt := a.applyCommand(cmd)
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInternal {
		t.Fatalf("failed queue commit receipt = %+v; want definite rejection", receipt)
	}
	if store.committedQueue != 0 {
		t.Fatalf("rejected input was durably accepted %d times", store.committedQueue)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.inbox.seenInputs) != 0 || len(a.conversation.durable.PendingInputViews()) != 0 {
		t.Fatalf("unaccepted input was published: %+v", a.conversation.durable.PendingInputViews())
	}
}

func (s *admissionBoundaryStore) CurrentCursor() session.Cursor {
	cursor := s.Store.CurrentCursor()
	if s.failNextRead {
		return cursor + 1
	}
	return cursor
}
