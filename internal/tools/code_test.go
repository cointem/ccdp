package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestRunCodeUnifiedResultExecutesOnce(t *testing.T) {
	for _, typed := range []bool{false, true} {
		calls := 0
		ctx := &Context{
			Args: map[string]any{"code": "text(1)", "timeout_ms": 1234},
			RunCode: func(code string, timeout time.Duration) (protocol.CodeResult, error) {
				calls++
				if code != "text(1)" || timeout != 1234*time.Millisecond {
					t.Fatalf("unexpected request: %q %s", code, timeout)
				}
				return protocol.CodeResult{Outputs: []json.RawMessage{json.RawMessage("1")}, Calls: 2}, nil
			},
		}
		var result protocol.CodeResult
		if typed {
			var err error
			executed, executeErr := (&RunCodeTool{}).Execute(ctx, ctx.Args)
			err = executeErr
			result = *executed.Code
			if err != nil {
				t.Fatal(err)
			}
		} else {
			out, err := executeToolText((&RunCodeTool{}), ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 1 || result.Calls != 2 || len(result.Outputs) != 1 || string(result.Outputs[0]) != "1" {
			t.Fatalf("calls=%d result=%+v", calls, result)
		}
	}
}

func TestRunCodeErrorsRemainInTypedResult(t *testing.T) {
	for _, ctx := range []*Context{
		{Args: map[string]any{"code": ""}},
		{Args: map[string]any{"code": "text(1)", "timeout_ms": 1}},
		{Args: map[string]any{"code": "text(1)"}},
		{Args: map[string]any{"code": "text(1)"}, RunCode: func(string, time.Duration) (protocol.CodeResult, error) {
			return protocol.CodeResult{}, errors.New("worker startup failed")
		}},
	} {
		out, err := executeToolText((&RunCodeTool{}), ctx)
		if err == nil || !json.Valid([]byte(out)) || !strings.Contains(out, err.Error()) {
			t.Fatalf("output=%q error=%v", out, err)
		}
	}
}
