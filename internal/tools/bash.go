package tools

import (
	"fmt"

	"ccdp/internal/execution"
)

// DefaultShellTimeout bounds a single Bash tool invocation.
const DefaultShellTimeout = execution.DefaultTimeout

// BashResult is the structured output of a shell invocation.
type BashResult struct {
	ExitCode  int
	Output    string
	Timeout   bool
	Truncated bool
}

// RunShell executes a command through the configured shell and the shared
// execution boundary. All callers, including custom commands and git tools,
// should use this path so strict sandbox policy cannot be bypassed.
func RunShell(ctx *Context, command string) (BashResult, error) {
	if ctx == nil {
		return BashResult{}, fmt.Errorf("bash: nil context")
	}
	if err := ctx.checkResources(); err != nil {
		return BashResult{}, err
	}
	res, err := execution.Run(execution.Request{
		Context:       ctx.Context,
		Command:       command,
		Dir:           ctx.WorkingDir,
		Timeout:       ctx.Timeout,
		Sandbox:       ctx.Sandbox,
		NotifyContext: ctx.notifyContext(),
		OutputLimit:   ctx.outputLimit(),
	})
	if err != nil {
		return BashResult{}, err
	}
	return BashResult{ExitCode: res.ExitCode, Output: res.Output, Timeout: res.TimedOut, Truncated: res.Truncated}, nil
}

// BashTool runs shell commands. It is the agent's main ability to interact
// with the real environment.
type BashTool struct{}

// NewBashTool creates the bash tool.
func NewBashTool() *BashTool { return &BashTool{} }

func (b *BashTool) Name() string { return "Bash" }

func (b *BashTool) Description() string {
	return "Start one /bin/sh command (use $? for exit status). Wait up to yield_ms (default 1000) and return its process_id, output and state. Continue via Process; never rerun merely to wait. timeout_ms is the execution deadline (default 120000; explicit 0 means none). stdin defaults to closed (immediate EOF); use stdin=pipe for Process.write or tty=true for a terminal, not both. TMPDIR is a private writable scratch directory; put temporary compiler outputs there or in the workspace, not at hard-coded /tmp paths. HOME is isolated: a missing toolchain configuration there does not prove the host lacks one. Additional access requires requested_capabilities approval."
}
func (b *BashTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"stdin":   map[string]any{"type": "string", "enum": []string{"closed", "pipe"}},
		"command": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "yield_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 3600000}, "tty": map[string]any{"type": "boolean"}, "requested_capabilities": capabilityRequestSchema()}, "required": []string{"command"}}
}
func (b *BashTool) Run(ctx *Context) (string, error) { return startCommand(ctx) }
