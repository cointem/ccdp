package tools

import (
	"fmt"
	"strings"

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
	return `Run a bash command. Use this tool to execute shell commands, scripts,
tests, build steps and inspect the environment. Returns stdout and stderr,
followed by the exit code. Long-running commands are interrupted after the
configured timeout (default 120s).`
}

func (b *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute. Use && between commands that must all succeed.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A short note describing what the command does and why. Useful for the log.",
			},
		},
		"required": []string{"command"},
	}
}

func (b *BashTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	command := StringArg(ctx.Args, "command", "")
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("bash: empty command")
	}
	res, err := RunShell(ctx, command)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(res.Output)
	if !strings.HasSuffix(res.Output, "\n") && res.Output != "" {
		sb.WriteString("\n")
	}
	if res.Timeout {
		fmt.Fprintf(&sb, "\nExit code: %d (timed out)\n", res.ExitCode)
	} else {
		fmt.Fprintf(&sb, "\nExit code: %d\n", res.ExitCode)
	}
	return boundedToolString(ctx, sb.String()), nil
}
