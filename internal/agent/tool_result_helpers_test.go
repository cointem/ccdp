package agent

import (
	"ccdp/internal/tools"
)

func executeToolText(tool tools.Tool, ctx *tools.Context) (string, error) {
	result, err := tool.Execute(ctx, ctx.Args)
	return result.Text, err
}
func executeToolResult(tool tools.Tool, ctx *tools.Context) (tools.Result, error) {
	return tool.Execute(ctx, ctx.Args)
}
