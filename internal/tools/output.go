package tools

import (
	"fmt"
)

type ReadOutputTool struct{}

func NewReadOutputTool() *ReadOutputTool { return &ReadOutputTool{} }
func (*ReadOutputTool) Name() string     { return "ReadOutput" }
func (*ReadOutputTool) Description() string {
	return "Read saved tool output by output_id, including full WebFetch text and retained logs of completed commands. offset and limit are bytes. Follow the returned offset to continue without rerunning the tool. Use Process to observe commands that are still running."
}
func (*ReadOutputTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"output_id": map[string]any{"type": "string", "minLength": 1},
		"offset":    map[string]any{"type": "integer", "minimum": 0, "default": 0},
		"limit":     map[string]any{"type": "integer", "minimum": 4, "maximum": 65536, "default": 16384},
	}, "required": []string{"output_id"}, "additionalProperties": false}
}
func (*ReadOutputTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
		if ctx.ReadOutput == nil {
			return "", fmt.Errorf("ReadOutput: session output reader is unavailable")
		}
		offset, err := IntArgChecked(args, "offset", 0)
		if err != nil {
			return "", err
		}
		limit, err := IntArgChecked(args, "limit", 16384)
		if err != nil {
			return "", err
		}
		page, err := ctx.ReadOutput(ctx, StringArg(args, "output_id", ""), int64(offset), limit)
		if err != nil {
			return "", err
		}
		// Keep continuation visible even when a requested page fills the output
		// budget. Data retains only source text, so its byte cursor never counts
		// the human-readable footer.
		for {
			footer := fmt.Sprintf("\n[bytes %d-%d of %d (end exclusive); EOF]", offset, page.Next, page.Total)
			if page.More {
				footer = fmt.Sprintf("\n[bytes %d-%d of %d (end exclusive); more content; continue with ReadOutput output_id=%q offset=%d]", offset, page.Next, page.Total, StringArg(args, "output_id", ""), page.Next)
			}
			available := ctx.outputLimit() - len(footer)
			if available < 0 {
				return "", fmt.Errorf("ReadOutput: output budget too small for pagination metadata")
			}
			if len(page.Text) <= available {
				ctx.recordData(page, page.More)
				return page.Text + footer, nil
			}
			page.Text = truncateUTF8(page.Text, available)
			if page.Text == "" {
				return "", fmt.Errorf("ReadOutput: output budget too small for metadata and content")
			}
			page.Next = int64(offset) + int64(len(page.Text))
			page.More = page.Next < page.Total
		}
	})
}
