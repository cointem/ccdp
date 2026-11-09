package mcp

import (
	"ccdp/internal/tools"
)

func executeToolText(tool tools.Tool, ctx *tools.Context) (string, error) {
	result, err := tool.Execute(ctx, ctx.Args)
	return result.Text, err
}
