package tools

import "testing"

func executeToolText(tool Tool, ctx *Context) (string, error) {
	if ctx == nil {
		ctx = &Context{}
	}
	result, err := tool.Execute(ctx, ctx.Args)
	return result.Text, err
}
func executeToolResult(tool Tool, ctx *Context) (Result, error) { return tool.Execute(ctx, ctx.Args) }

func runToolData(t *testing.T, tool Tool, ctx *Context, args map[string]any) (string, error) {
	t.Helper()
	result, err := tool.Execute(ctx, args)
	return string(result.Data), err
}
