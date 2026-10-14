package session

import (
	"math"
	"testing"
)

func TestRequestReservationIsValidatedAndFrozenByJournal(t *testing.T) {
	manifest := RequestManifest{RequestID: "request", SessionID: "session", Model: "model", Provider: "provider", AdapterVersion: "v1", Digest: "digest", Messages: []Message{testMessage("prompt", "hello")},
		ReservedUsage: &Usage{InputTokens: 100, OutputTokens: 200, TotalTokens: 300, TurnCount: 1},
	}
	for _, invalid := range []Usage{
		{InputTokens: 100, OutputTokens: 200, TotalTokens: 100},
		{InputTokens: math.MaxInt64, OutputTokens: 1, TotalTokens: 0},
		{InputTokens: 100, OutputTokens: 200, TotalTokens: 300, CachedTokens: 20},
	} {
		copy := manifest
		copy.ReservedUsage = &invalid
		if err := copy.validate(); err == nil {
			t.Fatalf("accepted invalid settlement bound: %+v", invalid)
		}
	}
	store := NewMemoryStore()
	defer store.Close()
	if _, err := store.Commit(Beginning, Batch{TransactionID: "prepared", Events: []Event{RequestPrepared{Manifest: manifest}}}); err != nil {
		t.Fatal(err)
	}

	manifest.ReservedUsage.InputTokens = 0
	rows, err := store.Read(Beginning)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].Event.(*RequestPrepared).Manifest.ReservedUsage; got.TotalTokens != 300 {
		t.Fatalf("caller mutated durable bound: %+v", got)
	}
}
