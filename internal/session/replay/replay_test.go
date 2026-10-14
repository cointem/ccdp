package replay

import (
	"math"
	"reflect"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

func TestIncrementalValuesMatchStoreReplay(t *testing.T) {
	store := session.NewMemoryStore()
	defer store.Close()
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	batches := []session.Batch{
		{TransactionID: "created", Events: []session.Event{session.SessionCreated{SessionID: "test", FormatVersion: session.SchemaVersion, Source: "test", CreatedAt: now}}},
		{TransactionID: "queued", Events: []session.Event{session.InputQueued{InputID: "input", MessageID: "user", Text: "hello", Strategy: "followup", CreatedAt: now}}},
		{TransactionID: "delivered", Events: []session.Event{session.InputDelivered{InputID: "input", TurnID: "turn-1"}}},
		{TransactionID: "answer", Events: []session.Event{session.AssistantCommitted{TurnID: "turn-1", StepID: "step-1", Message: session.Message{MessageID: "answer", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "hello"}}}}}},
	}
	incremental := New()
	for _, batch := range batches {
		result, err := store.Commit(store.CurrentCursor(), batch)
		if err != nil {
			t.Fatal(err)
		}
		if result.Applied {
			for _, event := range batch.Events {
				if err := incremental.Apply(event); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	records, err := store.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := RecordsRaw(records)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental, replayed) {
		t.Fatalf("incremental=%+v replay=%+v", incremental, replayed)
	}
}

func TestRecoveryDoesNotContaminateRawProjection(t *testing.T) {
	records := []session.Record{
		{Event: &session.AssistantCommitted{TurnID: "turn-1", StepID: "step-1", Message: session.Message{MessageID: "assistant", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentToolCall, ToolCall: &session.ToolCall{CallID: "call", ToolID: "Read", Arguments: []byte(`{}`)}}}}}},
		{Event: &session.ToolStarted{TurnID: "turn-1", StepID: "step-1", Call: session.ToolCall{CallID: "call", ToolID: "Read", Arguments: []byte(`{}`)}}},
	}
	raw, err := RecordsRaw(records)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := Records(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.History) != 1 || len(recovered.History) != 2 {
		t.Fatalf("raw=%d recovered=%d", len(raw.History), len(recovered.History))
	}
	if err := raw.Apply(session.ToolFinished{TurnID: "turn-1", StepID: "step-1", CallID: "call", Result: session.ToolResult{CallID: "call", Status: "success", Text: "actual"}}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Apply(session.ToolResultsProjected{TurnID: "turn-1", StepID: "step-1", Results: []session.ProjectedToolResult{{CallID: "call", Status: "success", Text: "actual"}}}); err != nil {
		t.Fatal(err)
	}
	if len(raw.History) != 2 || raw.History[1].Role != messages.RoleTool || raw.History[1].Content != "actual" {
		t.Fatalf("raw contaminated by recovery: %+v", raw.History)
	}
}

func TestNilFactsReturnError(t *testing.T) {
	var missing *session.SessionCreated
	for _, event := range []session.Event{nil, missing} {
		if err := New().Apply(event); err == nil {
			t.Fatal("nil event accepted")
		}
	}
}

func TestRecoveryCloneOwnsMutableState(t *testing.T) {
	raw := New()
	settings := session.Settings{AlwaysAllow: []string{"Read"}}
	tools := []session.ToolSchema{{ToolID: "Read", Schema: []byte(`{}`), Effects: []string{"read"}}}
	for _, e := range []session.Event{
		session.SettingsChanged{Settings: settings}, session.ToolsDiscovered{Tools: tools},
		session.InputQueued{InputID: "input", Text: "hello", Attachments: []session.BlobRef{{Hash: "original"}}},
		session.ToolStarted{TurnID: "turn-1", StepID: "step-1", Call: session.ToolCall{CallID: "call", ToolID: "Read", Arguments: []byte(`{}`)}},
	} {
		if err := raw.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	settings.AlwaysAllow[0] = "Write"
	tools[0].Schema[0] = '['
	tools[0].Effects[0] = "write"
	if raw.Settings.AlwaysAllow[0] != "Read" || string(raw.Discovered["Read"].Schema) != "{}" {
		t.Fatal("retained caller-owned event data")
	}
	recovered := raw.Recovered()
	recovered.Settings.AlwaysAllow[0] = "Write"
	schema := recovered.Discovered["Read"]
	schema.Schema[0] = '['
	schema.Effects[0] = "write"
	input := recovered.pendingInput["input"]
	input.Attachments[0].Hash = "changed"
	for k, v := range recovered.startedTools {
		v.Call.Arguments[0] = '['
		recovered.startedTools[k] = v
	}
	if raw.Settings.AlwaysAllow[0] != "Read" || string(raw.Discovered["Read"].Schema) != "{}" || raw.Discovered["Read"].Effects[0] != "read" || raw.pendingInput["input"].Attachments[0].Hash != "original" {
		t.Fatal("recovery aliases raw state")
	}
	for _, v := range raw.startedTools {
		if string(v.Call.Arguments) != "{}" {
			t.Fatal("recovery aliases tool arguments")
		}
	}
	if len(raw.History) != 0 || len(recovered.History) != 1 {
		t.Fatalf("history raw=%d recovered=%d", len(raw.History), len(recovered.History))
	}
}

func TestUsageOverflowLeavesProjectionUnchanged(t *testing.T) {
	raw := New()
	raw.Usage.InputTokens = math.MaxInt64
	before := raw.Clone()
	err := raw.Apply(session.AttemptFinished{Attempt: session.Attempt{TurnID: "turn-99", Usage: session.Usage{InputTokens: 1, OutputTokens: 2}}})
	if err == nil {
		t.Fatal("overflow accepted")
	}
	if !reflect.DeepEqual(before, raw) {
		t.Fatal("overflow partially changed projection")
	}
}
