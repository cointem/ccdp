package tools

import "encoding/json"

// executeText is an implementation helper, not a second tool interface.
// It collects text and declared data from a single operation.
func executeText(ctx *Context, args map[string]any, execute func(*Context) (string, error)) (Result, error) {
	var result Result
	if ctx == nil {
		ctx = &Context{}
	}
	local := ctx.WithArgs(args)
	local.dataResult = &result
	text, err := execute(local)
	result.Text = text
	return result, err
}
func (ctx *Context) recordData(value any, truncated bool) {
	if ctx.dataResult != nil {
		var err error
		ctx.dataResult.Data, err = json.Marshal(value)
		if err != nil {
			ctx.dataResult.DataError = err.Error()
		}
		ctx.dataResult.DataTruncated = truncated
	}
}
func objectOutput(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties}
}
func scalarOutput(kind string) map[string]any { return map[string]any{"type": kind} }
func arrayOutput(item any) map[string]any     { return map[string]any{"type": "array", "items": item} }
func (*ReadTool) OutputSchema() map[string]any {
	return objectOutput(map[string]any{
		"path": scalarOutput("string"), "lines": arrayOutput(objectOutput(map[string]any{"number": scalarOutput("integer"), "text": scalarOutput("string")})),
		"preview": scalarOutput("string"),
		"offset":  scalarOutput("integer"), "end": scalarOutput("integer"), "next_offset": scalarOutput("integer"), "eof": scalarOutput("boolean"), "truncated": scalarOutput("boolean"),
	})
}
func (*ReadOutputTool) OutputSchema() map[string]any {
	return objectOutput(map[string]any{"text": scalarOutput("string"), "next": scalarOutput("integer"), "total": scalarOutput("integer"), "more": scalarOutput("boolean")})
}
func (*GlobTool) OutputSchema() map[string]any {
	return objectOutput(map[string]any{"paths": arrayOutput(scalarOutput("string")), "total": scalarOutput("integer"), "truncated": scalarOutput("boolean")})
}
func (*LSTool) OutputSchema() map[string]any {
	return objectOutput(map[string]any{"path": scalarOutput("string"), "entries": arrayOutput(objectOutput(map[string]any{"name": scalarOutput("string"), "is_dir": scalarOutput("boolean"), "size": scalarOutput("integer")})), "total": scalarOutput("integer"), "truncated": scalarOutput("boolean")})
}
func (*RgTool) OutputSchema() map[string]any {
	return objectOutput(map[string]any{"mode": scalarOutput("string"), "entries": arrayOutput(objectOutput(map[string]any{"path": scalarOutput("string"), "line": scalarOutput("integer"), "text": scalarOutput("string"), "match": scalarOutput("boolean"), "count": scalarOutput("integer")})), "offset": scalarOutput("integer"), "next_offset": scalarOutput("integer"), "truncated": scalarOutput("boolean")})
}

// Process tools already own a documented JSON protocol. Parse only that
// protocol, never arbitrary output returned by other tools.
func processResult(result Result) Result {
	if json.Valid([]byte(result.Text)) {
		result.Data = json.RawMessage(result.Text)
		var metadata struct {
			Truncated bool `json:"truncated"`
		}
		_ = json.Unmarshal(result.Data, &metadata)
		result.DataTruncated = metadata.Truncated
	}
	return result
}
func (*BashTool) OutputSchema() map[string]any    { return processOutputSchema() }
func (*ProcessTool) OutputSchema() map[string]any { return processOutputSchema() }
func processOutputSchema() map[string]any {
	return objectOutput(map[string]any{"process_id": scalarOutput("integer"), "status": scalarOutput("string"), "exit_code": scalarOutput("integer"), "output": scalarOutput("string"), "offset": scalarOutput("integer"), "next_offset": scalarOutput("integer"), "log_path": scalarOutput("string"), "truncated": scalarOutput("boolean"), "warning": scalarOutput("string")})
}
