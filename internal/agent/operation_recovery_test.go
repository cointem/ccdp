package agent

import (
	"context"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestReadInputResultsUseDeliveredTurnOutcome(t *testing.T) {
	records := []session.Record{
		{Event: &session.InputDelivered{InputID: "first", TurnID: "turn-1"}},
		{Event: &session.TurnFinished{TurnID: "turn-1", Outcome: "error", Error: "first failed"}},
		{Event: &session.InputDelivered{InputID: "second", TurnID: "turn-2"}},
		{Event: &session.TurnFinished{TurnID: "turn-2", Outcome: "success"}},
		{Event: &session.InputDelivered{InputID: "running", TurnID: "turn-3"}},
		{Event: &session.InputCancelled{InputID: "cancelled", Reason: "queue cleared"}},
	}
	for _, test := range []struct {
		id        protocol.InputID
		delivered bool
		turn      protocol.TurnID
		status    protocol.TurnOutcomeStatus
		err       string
	}{
		{"first", true, "turn-1", protocol.TurnFailed, "first failed"},
		{"second", true, "turn-2", protocol.TurnSucceeded, ""},
		{"running", true, "turn-3", "", ""},
		{"cancelled", false, "", protocol.TurnCancelled, "queue cleared"},
		{"queued", false, "", "", ""},
	} {
		result, err := readInputResult(context.Background(), records, test.id)
		if err != nil || result.Delivered != test.delivered || result.TurnID != test.turn ||
			result.Status != test.status || result.Error != test.err {
			t.Fatalf("%s: %+v, %v", test.id, result, err)
		}
	}
}

func TestReadOperationResultPreservesRejectedCode(t *testing.T) {
	result, err := readOperationResult(context.Background(), []session.Record{{
		Event: &session.CommandCompleted{CommandID: "rejected", Outcome: "rejected",
			Code: string(protocol.ErrorClosed), Report: "cancelled"},
	}}, nil, "rejected")
	if err != nil || !result.Complete || result.Code != protocol.ErrorClosed || result.Error != "cancelled" {
		t.Fatalf("rejected result=%+v err=%v", result, err)
	}
}

var _ protocol.OperationReader = (*Agent)(nil)
var _ protocol.OperationReader = (*managedClient)(nil)
