package tools

// BashTool runs shell commands. It is the agent's main ability to interact
// with the real environment.
type BashTool struct{}

// NewBashTool creates the bash tool.
func NewBashTool() *BashTool { return &BashTool{} }

func (b *BashTool) Name() string { return "Bash" }

func (b *BashTool) Description() string {
	return "Run one /bin/sh command and wait for completion while the UI streams progress. Each call starts in cwd or the workspace; cd and exports do not persist. Use background=true for servers, watchers or independent work: return process_id immediately and receive completion at the next model boundary, without polling. stdin=pipe or tty=true also runs in the background for Process.write interaction; do not combine them. stdin defaults to closed. timeout_ms is the execution limit: foreground defaults to the configured timeout (normally 120000 ms), background has no default deadline; explicit 0 means none. Use the supplied continuation only when output is incomplete; never rerun a command to recover output or wait. Use TMPDIR or the workspace for temporary files. Tool caches are writable; credentials and private session state are protected. Additional access uses requested_capabilities and resumes this call after a decision; do not retry denied access."
}
func (b *BashTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"stdin":   map[string]any{"default": "closed", "type": "string", "enum": []string{"closed", "pipe"}},
		"command": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "background": map[string]any{"default": false, "type": "boolean"}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 3600000}, "tty": map[string]any{"default": false, "type": "boolean"}, "requested_capabilities": capabilityRequestSchema()}, "required": []string{"command"}}
}
func (b *BashTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	result, err := executeText(ctx, args, func(ctx *Context) (string, error) { return startCommand(ctx) })
	return processResult(result), err
}
