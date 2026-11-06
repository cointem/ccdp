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
	return `Execute JavaScript to compose currently advertised tools without an intervening model call. Use await tools.Read({...}) or tools["exact_tool_name"]({...}); each returns {status,text,data?,truncated,data_error?}. Call text(JSON_value) to emit a concise result. Only emitted values and a host summary return to the model; child calls are logged separately. Tool failures reject with ToolError {call_id,status,kind,result}; catch or use Promise.allSettled. A data_error can reject after execution succeeded: do not repeat a write merely because its returned data is invalid. Integers outside JavaScript's exact range require direct tool calls. Always await every tool call. File writes, Bash, network and MCP still pass normal permission checks; Promise.all does not grant concurrency to writes. Tools for session/goal/user-input/subagent control, ToolSearch and RunCode itself are unavailable inside code. First use ToolSearch directly for deferred tools. No import, require, fetch, filesystem or process globals. Fresh runtime each call; 64 KiB code, 1024 call-stack depth, 96 MiB soft Go memory target with a sampled 128 MiB worker heap budget (not a hard allocation cap), 5 seconds active JS, 128 calls, 32 queued, 512 KiB per result and 8 MiB total results and emitted data. Emit concise output: model output budgets may omit values or the entire display when exhausted; execution status and saved output remain available. Use direct tools for native media.`
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
