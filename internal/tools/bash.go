package tools

// BashTool runs shell commands. It is the agent's main ability to interact
// with the real environment.
type BashTool struct{}

// NewBashTool creates the bash tool.
func NewBashTool() *BashTool { return &BashTool{} }

func (b *BashTool) Name() string { return "Bash" }

func (b *BashTool) Description() string {
	return "Start one /bin/sh command (use $? for exit status). Each call starts in cwd or the session workspace; cd and exports do not persist to later tools. Use absolute paths to pass created files to Read. Wait up to yield_ms (default 1000) and return its process_id, output and state. Continue via Process; never rerun merely to wait. If output is truncated, use Process.read with next_offset (bytes), or Read the log_path (lines). timeout_ms overrides the configured command timeout (normally 120000 ms; explicit 0 means none). stdin defaults to closed (immediate EOF); use stdin=pipe for Process.write or tty=true for a terminal, not both. TMPDIR is a private session-scoped writable scratch directory; put temporary compiler outputs there or in the workspace, not at hard-coded /tmp paths. HOME is isolated: a missing toolchain configuration there does not prove the host lacks one. Additional access requires requested_capabilities approval."
}
func (b *BashTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"stdin":   map[string]any{"default": "closed", "type": "string", "enum": []string{"closed", "pipe"}},
		"command": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "yield_ms": map[string]any{"default": 1000, "type": "integer", "minimum": 0, "maximum": 10000}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 3600000}, "tty": map[string]any{"default": false, "type": "boolean"}, "requested_capabilities": capabilityRequestSchema()}, "required": []string{"command"}}
}
func (b *BashTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	result, err := executeText(ctx, args, func(ctx *Context) (string, error) { return startCommand(ctx) })
	return processResult(result), err
}
