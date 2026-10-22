package agent

import (
	"context"
	"reflect"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func historyCommandCases() []protocol.Command {
	return []protocol.Command{
		{ID: "clear", Type: protocol.CommandClearConversation},
		{ID: "remove", Type: protocol.CommandRemoveMessages, Remove: &protocol.RemoveMessages{Count: 1}},
		{ID: "rewind", Type: protocol.CommandRewindConversation, Rewind: &protocol.RewindConversation{Count: 1}},
		{ID: "remove-zero", Type: protocol.CommandRemoveMessages, Remove: &protocol.RemoveMessages{}},
		{ID: "rewind-unchanged", Type: protocol.CommandRewindConversation, Rewind: &protocol.RewindConversation{Count: 3}},
	}
}

func TestHistoryCommandFailureDoesNotPublishEdit(t *testing.T) {
	for _, cmd := range historyCommandCases() {
		t.Run(string(cmd.ID), func(t *testing.T) {
			a, _ := sessionRepairAgent(t, false)
			sessionRepairSeed(t, a, sessionRepairHistory(3))
			cmd.SessionID = protocol.SessionID(a.SessionID())
			before := a.History()
			a.mu.Lock()
			a.turnMemory.tokenBaseline.promptTokens = 123
			a.turnMemory.tokenBaseline.historyLen = 3
			a.mu.Unlock()
			backend := journalBackend(t, a.persistenceHandle())
			backend.mu.Lock()
			backend.store = requestJournalFailEventStore{Store: backend.store, event: session.EventTypeCommandCompleted}
			backend.mu.Unlock()

			receipt, err := a.Submit(context.Background(), cmd)
			if err != nil || !receipt.Rejected() {
				t.Fatalf("failed transaction receipt=%+v err=%v", receipt, err)
			}
			if !reflect.DeepEqual(a.History(), before) {
				t.Fatal("completion failure committed the history edit")
			}
			a.mu.Lock()
			unchanged := a.turnMemory.tokenBaseline.promptTokens == 123 && a.turnMemory.tokenBaseline.historyLen == 3
			a.mu.Unlock()
			if !unchanged {
				t.Fatal("failed edit cleared the token baseline")
			}
			rows, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				switch event := row.Event.(type) {
				case *session.CommandScheduled:
					if event.CommandID == string(cmd.ID) {
						t.Fatal("failed atomic command left an admission")
					}
				case *session.CommandCompleted:
					if event.CommandID == string(cmd.ID) {
						t.Fatal("failed atomic command left a completion")
					}
				}
			}
		})
	}
}

func TestHistoryCommandLostReceiptDoesNotRepeatAfterResume(t *testing.T) {
	for _, cmd := range historyCommandCases() {
		t.Run(string(cmd.ID), func(t *testing.T) {
			a, cfg := sessionRepairAgent(t, false)
			sessionRepairSeed(t, a, sessionRepairHistory(3))
			cmd.SessionID = protocol.SessionID(a.SessionID())
			cmd.ExpectedRevision = a.revision().LogSeq
			// Stop after the mutation commit, before dispatch publishes a receipt.
			var err error
			switch cmd.Type {
			case protocol.CommandClearConversation:
				err = a.clearHistoryCommand(cmd)
			case protocol.CommandRemoveMessages:
				err = a.removeMessagesCommand(cmd)
			case protocol.CommandRewindConversation:
				err = a.rewindMessagesCommand(cmd)
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			var admissionTx, resetTx, completionTx string
			completions := 0
			for _, row := range rows {
				switch event := row.Event.(type) {
				case *session.CommandScheduled:
					if event.CommandID == string(cmd.ID) {
						admissionTx = row.TransactionID
					}
				case *session.ConversationReset:
					if event.TurnID == string(cmd.ID) {
						resetTx = row.TransactionID
					}
				case *session.CommandCompleted:
					if event.CommandID == string(cmd.ID) {
						completionTx = row.TransactionID
						completions++
					}
				}
			}
			if admissionTx == "" || admissionTx != resetTx || resetTx != completionTx || completions != 1 {
				t.Fatalf("history command was not one transaction: %q %q %q count=%d", admissionTx, resetTx, completionTx, completions)
			}
			if err := a.CloseContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := LoadSession(cfg.SessionDir, a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Resume(&cfg, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "new answer after reopening"}); err != nil {
				t.Fatal(err)
			}
			before := reopened.History()
			receipt, err := reopened.Submit(context.Background(), cmd)
			if err != nil || receipt.Status != protocol.ReceiptApplied || !reflect.DeepEqual(reopened.History(), before) {
				t.Fatalf("retry repeated the edit: receipt=%+v err=%v history=%+v", receipt, err, reopened.History())
			}
			stale := cmd
			stale.ID = "new-stale-command"
			receipt, err = reopened.Submit(context.Background(), stale)
			if err != nil || receipt.Error == nil || receipt.Error.Code != protocol.ErrorStaleRevision {
				t.Fatalf("new command bypassed revision guard: %+v %v", receipt, err)
			}
			conflict := protocol.Command{ID: cmd.ID, SessionID: cmd.SessionID, Type: protocol.CommandRemoveMessages, Remove: &protocol.RemoveMessages{Count: 999}}
			receipt, err = reopened.Submit(context.Background(), conflict)
			if err != nil || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
				t.Fatalf("changed command was not rejected: %+v %v", receipt, err)
			}
		})
	}
}

func TestHistoryCommandPublishesOnlyOneCompletion(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	cmd := protocol.Command{ID: "empty-clear", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandClearConversation}
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("empty clear: %+v %v", receipt, err)
	}
	rows, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	completions := 0
	for _, row := range rows {
		if event, ok := row.Event.(*session.CommandCompleted); ok && event.CommandID == string(cmd.ID) {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("dispatch submitted completion %d times", completions)
	}
}
