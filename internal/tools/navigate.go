package tools

import (
	"encoding/json"
	"fmt"
)

type CodeNavigateTool struct{}

func NewCodeNavigateTool() *CodeNavigateTool { return &CodeNavigateTool{} }
func (*CodeNavigateTool) Name() string       { return "CodeNavigate" }
func (*CodeNavigateTool) Description() string {
	return "Ask a configured language server for definitions, references or document symbols. Positions use 1-based lines and zero-based UTF-16 characters. Without a server use Grep and Read. No text candidate is presented as semantic navigation."
}
func (*CodeNavigateTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"operation": map[string]any{"type": "string", "enum": []string{"definition", "references", "outline"}}, "path": map[string]any{"type": "string"}, "line": map[string]any{"type": "integer", "minimum": 1}, "character": map[string]any{"type": "integer", "minimum": 0}, "cursor": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "required": []string{"operation", "path"}}
}
func (*CodeNavigateTool) Run(ctx *Context) (string, error) {
	if e := ctx.checkResources(); e != nil {
		return "", e
	}
	op := StringArg(ctx.Args, "operation", "")
	if op != "definition" && op != "references" && op != "outline" {
		return "", fmt.Errorf("unsupported semantic operation; use Glob/Grep for text search")
	}
	path := StringArg(ctx.Args, "path", "")
	if path == "" {
		return "", fmt.Errorf("path required")
	}
	if _, e := ctx.ResolveRead(path); e != nil {
		return "", e
	}
	line, e := boundedInt(ctx.Args, "line", 1, 1, int(^uint(0)>>1))
	if e != nil {
		return "", e
	}
	character, e := boundedInt(ctx.Args, "character", 0, 0, int(^uint(0)>>1))
	if e != nil {
		return "", e
	}
	if ctx.NavigateSemantic == nil {
		return "", fmt.Errorf("no language server; use Grep/Read")
	}
	raw, e := ctx.NavigateSemantic(op, path, line, character)
	if e != nil {
		return "", e
	}
	return paginateLocations(ctx, raw)
}

func paginateLocations(ctx *Context, raw string) (string, error) {
	var payload struct {
		Locations []json.RawMessage `json:"locations"`
	}
	if e := json.Unmarshal([]byte(raw), &payload); e != nil {
		return "", e
	}
	offset, e := boundedInt(ctx.Args, "cursor", 0, 0, int(^uint(0)>>1))
	if e != nil {
		return "", e
	}
	limit, e := boundedInt(ctx.Args, "limit", 50, 1, 100)
	if e != nil {
		return "", e
	}
	total := len(payload.Locations)
	offset = min(offset, total)
	end := min(offset+limit, total)
	for {
		next := 0
		if end < total {
			next = end
		}
		b, e := json.Marshal(map[string]any{"source": "lsp", "position_encoding": "utf-16", "locations": payload.Locations[offset:end], "total": total, "next": next})
		if e != nil {
			return "", e
		}
		if ctx.OutputLimit <= 0 || len(b) <= ctx.OutputLimit {
			return string(b), nil
		}
		if end-offset <= 1 {
			return "", fmt.Errorf("location exceeds output budget")
		}
		end--
	}
}
