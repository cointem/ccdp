package tools

import (
	"encoding/json"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

type RunCodeTool struct{}

func (*RunCodeTool) Name() string { return "RunCode" }
func (*RunCodeTool) Description() string {
	return `Execute JavaScript to compose advertised tools without intervening model calls. Use await tools.Read({...}) or tools["exact_tool_name"]({...}); results contain {status,text,data?,truncated,data_error?,failure?}. Emit concise values with text(value); child calls are logged separately. Failures throw ToolError {call_id,status,kind,result}; catch or use Promise.allSettled. result.failure reports known side_effect and retry_class. Never repeat a mutation with completed/unknown side effects or never_replay; invalid returned data also does not undo execution. Always await calls. Independent reads can run in parallel; writes remain ordered and all calls retain normal permission checks. Session/goal/user-input/subagent controls, ToolSearch and RunCode cannot be called here; discover deferred tools directly first. No import, require, fetch, filesystem or process globals. Limits: 64 KiB code, 5 seconds active JS, 128 calls, 32 queued, 512 KiB per result and 8 MiB total results and emitted data. Model output budgets may shorten or omit emitted values; execution status and saved output remain available. Use direct calls for native media and integers outside JavaScript's exact range.`
}
func (*RunCodeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"default": 120000, "type": "integer", "minimum": 1000, "maximum": 300000}}, "required": []string{"code"}, "additionalProperties": false,
	}
}
func (t *RunCodeTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	result, err := t.executeCode(ctx.WithArgs(args))
	b, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return Result{}, encodeErr
	}
	return Result{Text: string(b), Code: &result}, err
}

func (*RunCodeTool) executeCode(ctx *Context) (result protocol.CodeResult, err error) {
	result.Outputs = []json.RawMessage{}
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	code, ok := ctx.Args["code"].(string)
	if !ok || len(code) == 0 || len(code) > 64<<10 {
		return result, fmt.Errorf("RunCode: code must contain 1..65536 bytes")
	}
	n, err := IntArgChecked(ctx.Args, "timeout_ms", 120000)
	if err != nil || n < 1000 || n > 300000 {
		return result, fmt.Errorf("RunCode: timeout_ms must be 1000..300000")
	}
	if ctx.RunCode == nil {
		return result, fmt.Errorf("RunCode: execution host is unavailable")
	}
	return ctx.RunCode(code, time.Duration(n)*time.Millisecond)
}
