package session

import (
	"reflect"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestGoalChangedCodec(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	goal := &protocol.Goal{ID: "goal", Objective: "complete work", Status: protocol.GoalBudgetLimited, TokenBudget: 10, TokensUsed: 12, CreatedAt: now, UpdatedAt: now}
	for _, event := range []GoalChanged{{Goal: goal, AccountingID: "request-1"}, {Goal: nil}} {
		wire, err := encodeEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeEvent(wire)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, &event) {
			t.Fatalf("roundtrip: %#v != %#v", decoded, event)
		}
	}
	if _, err := encodeEvent(GoalChanged{AccountingID: "request"}); err == nil {
		t.Fatal("accepted accounting without goal")
	}
	goal.Status = "unknown"
	if _, err := encodeEvent(GoalChanged{Goal: goal}); err == nil {
		t.Fatal("accepted invalid goal")
	}
}
