package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

type validatedEffectTool struct {
	calls int
	seen  map[string]any
	data  json.RawMessage
}

func (*validatedEffectTool) Name() string        { return "ValidatedEffect" }
func (*validatedEffectTool) Description() string { return "test shared tool validation" }
func (*validatedEffectTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"id":    map[string]any{"type": "integer", "minimum": json.Number("9007199254740993")},
			"limit": map[string]any{"type": "integer", "default": 12},
		}, "required": []string{"id"}}
}
func (*validatedEffectTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}}
}
func (tool *validatedEffectTool) Execute(_ *tools.Context, args map[string]any) (tools.Result, error) {
	tool.calls++
	tool.seen = args
	return tools.Result{Text: "effect completed", Data: tool.data}, nil
}

func TestDirectAndProgramCallsShareExactValidationBeforeExecution(t *testing.T) {
	for _, program := range []bool{false, true} {
		t.Run(fmt.Sprint(program), func(t *testing.T) {
			a := capabilityTestAgent(t)
			tool := &validatedEffectTool{data: json.RawMessage(`{"value":1}`)}
			a.services.registry.Register(tool)
			a.services.perms.SetPolicy(permissions.Policy{AlwaysAllow: []string{tool.Name()}})
			step := testToolStep(t, a)
			journal := toolJournalContextForStep(step)
			journal.program = program
			if program {
				journal.parentCallID, journal.childIndex = "parent", 1
			}
			for i, id := range []any{"9007199254740993", nil, json.Number("9007199254740992")} {
				result := a.executeToolResultWithJournal(messages.ToolCall{ID: fmt.Sprintf("invalid-%d", i), Name: tool.Name(), Arguments: map[string]any{"id": id}}, journal)
				if !result.IsError || !strings.Contains(result.Output, "invalid tool arguments") || tool.calls != 0 {
					t.Fatalf("invalid call executed: %+v calls=%d", result, tool.calls)
				}
			}
			result := a.executeToolResultWithJournal(messages.ToolCall{ID: "valid", Name: tool.Name(), Arguments: map[string]any{"id": json.Number("9007199254740993")}}, journal)
			if result.IsError || result.Status != "success" || tool.calls != 1 || tool.seen["id"] != json.Number("9007199254740993") || tool.seen["limit"] != json.Number("12") {
				t.Fatalf("valid call failed or changed number: %+v calls=%d args=%#v", result, tool.calls, tool.seen)
			}
		})
	}
}

func TestInvalidOutputRecordsCompletedEffectAndNeverReexecutes(t *testing.T) {
	for _, program := range []bool{false, true} {
		t.Run(fmt.Sprint(program), func(t *testing.T) {
			a := capabilityTestAgent(t)
			tool := &validatedEffectTool{data: json.RawMessage(`{"value":"wrong"}`)}
			a.services.registry.Register(tool)
			a.services.perms.SetPolicy(permissions.Policy{AlwaysAllow: []string{tool.Name()}})
			journal := toolJournalContextForStep(testToolStep(t, a))
			journal.program = program
			if program {
				journal.parentCallID, journal.childIndex = "parent", 1
			}
			result := a.executeToolResultWithJournal(messages.ToolCall{ID: "bad-data", Name: tool.Name(), Arguments: map[string]any{"id": json.Number("9007199254740993")}}, journal)
			if result.IsError || result.Status != "success" || result.DataError == "" || tool.calls != 1 {
				t.Fatalf("data validation changed execution outcome: %+v calls=%d", result, tool.calls)
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			var completed *session.ToolFinished
			for _, record := range records {
				if event, ok := record.Event.(*session.ToolFinished); ok && event.CallID == "bad-data" {
					completed = event
				}
			}
			if completed == nil || completed.Status != "success" || completed.SideEffect != "completed" || completed.DataError == "" || completed.Data == nil {
				t.Fatalf("durable execution/data facts lost: %+v", completed)
			}
		})
	}
}

type invalidOutputSchemaTool struct{ *validatedEffectTool }

func (*invalidOutputSchemaTool) OutputSchema() map[string]any {
	return map[string]any{"$ref": "external-output.json"}
}

func TestSchemaCompilationErrorPreventsExecution(t *testing.T) {
	for _, program := range []bool{false, true} {
		t.Run(fmt.Sprint(program), func(t *testing.T) {
			a := capabilityTestAgent(t)
			effect := &validatedEffectTool{data: json.RawMessage(`{"value":1}`)}
			tool := &invalidOutputSchemaTool{effect}
			a.services.registry.Register(tool)
			a.services.perms.SetPolicy(permissions.Policy{AlwaysAllow: []string{tool.Name()}})
			journal := toolJournalContextForStep(testToolStep(t, a))
			journal.program = program
			if program {
				journal.parentCallID, journal.childIndex = "parent", 1
			}
			result := a.executeToolResultWithJournal(messages.ToolCall{ID: "bad-schema", Name: tool.Name(), Arguments: map[string]any{"id": json.Number("9007199254740993")}}, journal)
			if !result.IsError || result.Status != "error" || effect.calls != 0 || !strings.Contains(result.Output, "output schema") {
				t.Fatalf("invalid definition allowed an effect: result=%+v calls=%d", result, effect.calls)
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if started, ok := record.Event.(*session.ToolStarted); ok && started.Call.CallID == "bad-schema" {
					t.Fatal("invalid definition crossed the durable execution boundary")
				}
			}
		})
	}
}
