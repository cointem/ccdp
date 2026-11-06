package tooling

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func TestCodeOutputFitsSmallResultBudget(t *testing.T) {
	code := protocol.CodeResult{Outputs: []json.RawMessage{json.RawMessage("1")}}
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "parent"}}, []Result{{Code: &code, Status: "success"}}, 1024, 0, 0)
	want, _ := json.Marshal(code)
	if err != nil || budget.Truncated || rows[0].Output != string(want) || budget.Used != int64(len(want)) {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
}

func TestCodeOutputBudgetCountsArraySeparators(t *testing.T) {
	code := protocol.CodeResult{Calls: 4, Denied: 1, Processes: []string{"123"}, Outputs: make([]json.RawMessage, 20000)}
	for i := range code.Outputs {
		code.Outputs[i] = json.RawMessage("1")
	}
	before, _ := json.Marshal(code)
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "parent"}}, []Result{{Code: &code, Status: "success"}}, 32000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.CodeResult
	if err := json.Unmarshal([]byte(rows[0].Output), &got); err != nil {
		t.Fatal(err)
	}
	if !budget.Truncated || budget.Used != int64(len(rows[0].Output)) || len(rows[0].Output) > 32000 || len(got.Outputs) == 0 || len(got.Outputs) >= len(code.Outputs) || got.Calls != 4 || got.Denied != 1 || !reflect.DeepEqual(got.Processes, code.Processes) || got.Note == "" || rows[0].Status != "success" || rows[0].IsError {
		t.Fatalf("%+v budget=%+v outputs=%d bytes=%d", got, budget, len(got.Outputs), len(rows[0].Output))
	}
	after, _ := json.Marshal(code)
	if string(after) != string(before) {
		t.Fatal("projection changed the raw execution result")
	}
}

func TestCodeOutputBudgetCountsEscapedErrorAndPreservesHandles(t *testing.T) {
	code := protocol.CodeResult{
		Calls: 2, Failed: 1, Processes: []string{"4294967297"},
		Outputs: []json.RawMessage{json.RawMessage(`"` + strings.Repeat("x", 27500) + `"`)},
		Error:   strings.Repeat("出错<&>", 2000), Note: strings.Repeat("hook context ", 100),
	}
	before, _ := json.Marshal(code)
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "parent"}}, []Result{{Code: &code, IsError: true, Status: "error"}}, 1024, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.CodeResult
	if err := json.Unmarshal([]byte(rows[0].Output), &got); err != nil {
		t.Fatal(err)
	}
	if len("Error: ")+len(rows[0].Output) > 1024 || budget.Used != int64(len("Error: ")+len(rows[0].Output)) || !budget.Truncated || !utf8.ValidString(got.Error) || !strings.HasPrefix(got.Error, "出错") || !strings.HasSuffix(got.Error, "...") || got.Calls != 2 || got.Failed != 1 || !reflect.DeepEqual(got.Processes, code.Processes) || rows[0].Status != "error" || !rows[0].IsError {
		t.Fatalf("%+v %+v", got, budget)
	}
	after, _ := json.Marshal(code)
	if string(after) != string(before) {
		t.Fatal("projection changed the raw execution result")
	}
}

func TestOutputBudgetCountsErrorPrefixOnce(t *testing.T) {
	for _, result := range []Result{
		{Output: "abc", Status: "error", IsError: true},
		{Output: "abc", IsError: true},
		{Output: "abc", Status: "denied"},
		{Output: "Error: abc", Status: "error", IsError: true},
	} {
		rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "failed"}}, []Result{result}, 10, 10, 0)
		if err != nil || budget.Used != 10 || budget.Truncated || rows[0].Output != result.Output || rows[0].Status != result.Status || rows[0].IsError != result.IsError {
			t.Fatalf("input=%+v results=%+v budget=%+v error=%v", result, rows, budget, err)
		}
	}
}

func TestOutputBudgetKeepsExistingErrorPrefixWhenTruncating(t *testing.T) {
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "failed"}}, []Result{{Output: "Error: details", Status: "error"}}, 7, 7, 0)
	if err != nil || rows[0].Output != "Error: " || !budget.Truncated || budget.Used != 7 {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
}

func TestOutputBudgetOmitsTextWhenPrefixCannotFit(t *testing.T) {
	for _, result := range []Result{
		{Output: "details", Status: "cancelled"},
		{Output: "Error: details", Status: "error"},
		{IsError: true},
	} {
		rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "failed"}}, []Result{result}, 1024, 6, 0)
		if err != nil || len(rows) != 1 || rows[0].Output != "" || !rows[0].DisplayTruncated || !budget.Truncated || budget.Used != 0 {
			t.Fatalf("%+v %+v %v", rows, budget, err)
		}
	}
}

func TestCodeOutputBudgetIncludesErrorPrefixAtExactBoundary(t *testing.T) {
	code := protocol.CodeResult{Outputs: []json.RawMessage{}, Error: "e"}
	encoded, _ := json.Marshal(code)
	size := len("Error: ") + len(encoded)
	input := []Result{{Code: &code, Status: "error", IsError: true}}
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "failed"}}, input, size, int64(size), 0)
	if err != nil || rows[0].Output != string(encoded) || budget.Used != int64(size) || budget.Truncated {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
	rows, budget, err = ApplyOutputBudget([]messages.ToolCall{{ID: "failed"}}, input, size-1, int64(size-1), 0)
	if err != nil || len(rows) != 1 || rows[0].Output != "" || !budget.Truncated || budget.Used != 0 {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
}

func TestOutputBudgetIncludesErrorPrefixInAggregate(t *testing.T) {
	rows, budget, err := ApplyOutputBudget(
		[]messages.ToolCall{{ID: "first"}, {ID: "second"}},
		[]Result{{Output: "abc", Status: "success"}, {Output: "xyz", Status: "error"}},
		1024, 12, 0,
	)
	if err != nil || rows[0].Output != "abc" || rows[1].Output != "xy" || !budget.Truncated || budget.Used != 12 {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
}

func TestCodeSummaryCannotFitExhaustedAggregate(t *testing.T) {
	code := protocol.CodeResult{Outputs: []json.RawMessage{}, Calls: 1}
	encoded, _ := json.Marshal(code)
	calls := []messages.ToolCall{{ID: "first"}, {ID: "second"}}
	results := []Result{{Code: &code, Status: "success"}, {Code: &code, Status: "success"}}
	rows, budget, err := ApplyOutputBudget(calls, results, 1024, int64(len(encoded)), 0)
	if err != nil || len(rows) != 2 || rows[1].Output != "" || !rows[1].DisplayTruncated || budget.Used != int64(len(encoded)) || results[0].Status != "success" || results[1].Status != "success" || results[0].IsError || results[1].IsError {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
}

func TestOutputBudgetCombinesPerResultAndAggregateWithoutChangingStatus(t *testing.T) {
	calls := []messages.ToolCall{{ID: "first"}, {ID: "second"}}
	results := []Result{{Output: strings.Repeat("好", 50), Status: "success"}, {Output: strings.Repeat("好", 50), Status: "success"}}
	rows, budget, err := ApplyOutputBudget(calls, results, 100, 130, 0)
	if err != nil || len(rows[0].Output) > 100 || len(rows[1].Output) > 30 || !utf8.ValidString(rows[0].Output) || !utf8.ValidString(rows[1].Output) || !budget.Truncated || budget.Used != int64(len(rows[0].Output)+len(rows[1].Output)) || budget.Used > 130 {
		t.Fatalf("%+v %+v %v", rows, budget, err)
	}
	for _, row := range rows {
		if row.Status != "success" || row.IsError {
			t.Fatal("display truncation changed execution status")
		}
	}
}

func TestOutputBudgetKeepsPreviewOfJSONText(t *testing.T) {
	rows, budget, err := ApplyOutputBudget([]messages.ToolCall{{ID: "parent"}}, []Result{{Output: `{"value":"` + strings.Repeat("large", 100) + `"}`, Status: "success"}}, 100, 0, 0)
	if err != nil || !budget.Truncated || rows[0].Output == "" || !rows[0].DisplayTruncated || rows[0].IsError || rows[0].Status != "success" || !strings.Contains(rows[0].Output, "ReadOutput") {
		t.Fatalf("%+v %+v", rows, budget)
	}
}

func TestProcessWriteProjectionKeepsAcknowledgement(t *testing.T) {
	p := protocol.ProcessOutput{ID: "p.test", Status: "input_sent", BytesSent: 7, EOF: true}
	rows, _, err := ApplyOutputBudget([]messages.ToolCall{{ID: "write"}}, []Result{{Process: &p, Status: "success"}}, 1024, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.ProcessOutput
	if err := json.Unmarshal([]byte(rows[0].Output), &got); err != nil || got.BytesSent != 7 || !got.EOF {
		t.Fatalf("write acknowledgement lost: %s %v", rows[0].Output, err)
	}
}
