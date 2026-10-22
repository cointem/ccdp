package tools

func executeToolText(tool Tool, ctx *Context) (string, error) {
	if ctx == nil {
		ctx = &Context{}
	}
	result, err := tool.Execute(ctx, ctx.Args)
	return result.Text, err
}
func executeToolResult(tool Tool, ctx *Context) (Result, error) { return tool.Execute(ctx, ctx.Args) }
