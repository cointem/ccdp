package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type childRecordBarrierStore struct {
	session.Store
	transition string
	entered    chan struct{}
	release    <-chan struct{}
	once       sync.Once
}

func (s *childRecordBarrierStore) Commit(cursor session.Cursor, batch session.Batch) (session.CommitResult, error) {
	result, err := s.Store.Commit(cursor, batch)
	matches := strings.HasSuffix(batch.TransactionID, "-"+s.transition)
	if s.transition == "outcome" {
		matches = strings.HasPrefix(batch.TransactionID, "child-outcome-")
	}
	if err == nil && result.Applied && matches {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return result, err
}

func TestSupervisorControlRemainsResponsiveDuringRecordAcknowledgement(t *testing.T) {
	for _, transition := range []string{"starting", "running", "outcome"} {
		t.Run(transition, func(t *testing.T) {
			p := &childRuntimeTestProvider{name: "settlement-barrier", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				return llm.StreamResult{Text: "answer", FinishReason: "stop", PromptTokens: 3}, nil
			}}
			a, _ := newChildRuntimeTestAgent(t, p)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			backend := journalBackend(t, a.persistenceHandle())
			backend.mu.Lock()
			backend.store = &childRecordBarrierStore{Store: backend.store, transition: transition, entered: entered, release: release}
			backend.mu.Unlock()
			r, err := a.services.supervisor.launch(a, a.lifecycle.rootCtx, childTask{Description: "settle"}, childPurposeTask, "call")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("child record did not reach acknowledgement barrier")
			}

			controlled := make(chan error, 1)
			go func() {
				_, err := a.Sessions().Control(context.Background(), protocol.AgentControl{
					ID: "cancel-during-record", SessionID: r.fact.Child.SessionID, Action: "cancel",
				})
				controlled <- err
			}()
			select {
			case err := <-controlled:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("child control waited on the parent's journal acknowledgement")
			}
			r.mu.Lock()
			accounted := r.fact.UsageAccounted
			r.mu.Unlock()
			if accounted {
				t.Fatal("child outcome was acknowledged before the journal receipt")
			}
			select {
			case <-r.done:
				t.Fatal("child settled before the journal receipt")
			default:
			}

			releaseOnce.Do(func() { close(release) })
			waitManaged(t, r)
			r.mu.Lock()
			status, accounted, runID := r.fact.Child.Run.Status, r.fact.UsageAccounted, r.fact.Child.Run.ID
			r.mu.Unlock()
			wantStatus := "cancelled"
			if transition == "outcome" {
				wantStatus = "succeeded"
			}
			if status != wantStatus || !accounted {
				t.Fatalf("settlement after acknowledgement: status=%s accounted=%v", status, accounted)
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			outcomes := 0
			for _, record := range records {
				if record.TransactionID == "child-outcome-"+string(runID) {
					outcomes++
				}
			}
			if outcomes != 1 {
				t.Fatalf("terminal record count = %d, want 1", outcomes)
			}
		})
	}
}
