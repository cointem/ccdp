package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestReviewSessionTitleRuneBoundary(t *testing.T) {
	got := sessionTitle([]messages.Message{{Role: messages.RoleUser, Content: strings.Repeat("a", 59) + "中"}})
	if !utf8.ValidString(got) {
		t.Fatalf("session title has invalid UTF-8: %q", got)
	}
}

func TestReviewScheduledCommandsReachTerminalReceipt(t *testing.T) {
	for _, kind := range []protocol.CommandType{protocol.CommandCompact, protocol.CommandFork} {
		t.Run(string(kind), func(t *testing.T) {
			a := newJournalRuntimeAgent(t, &formalCompletionProvider{})
			cmd := protocol.Command{ID: protocol.CommandID("review-" + string(kind)), SessionID: protocol.SessionID(a.SessionID()), Type: kind}
			if kind == protocol.CommandFork {
				cmd.Fork = &protocol.Fork{Count: -1}
			}
			r, err := a.Submit(context.Background(), cmd)
			if err != nil || r.Status != protocol.ReceiptScheduled {
				t.Fatalf("admission = %+v, %v", r, err)
			}
			done := make(chan struct{})
			go func() { a.operationWG.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not settle")
			}
			a.mu.Lock()
			final := a.seenReceipts[cmd.ID]
			a.mu.Unlock()
			if final.Status == protocol.ReceiptScheduled {
				t.Errorf("worker settled but receipt remained scheduled: %+v", final)
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			var completed bool
			for _, record := range records {
				if fact, ok := record.Event.(*session.CommandCompleted); ok && fact.CommandID == string(cmd.ID) {
					completed = true
				}
			}
			if !completed {
				t.Fatal("worker settled without durable CommandCompleted")
			}
		})
	}
}
