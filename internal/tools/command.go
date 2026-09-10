package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CommandTool is a user-defined external tool (Codex's config tools). When the
// model calls it, the configured shell command runs with the tool arguments
// (a JSON object) piped to stdin; stdout+stderr become the tool result.
type CommandTool struct {
	name        string
	description string
	command     string
	schema      map[string]any
}

// NewCommandTool wraps a user tool spec into a tools.Tool.
func NewCommandTool(name, description, command string, schema map[string]any) *CommandTool {
	if description == "" {
		description = fmt.Sprintf("Run the configured external command %q.", name)
	}
	if schema == nil {
		schema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return &CommandTool{name: name, description: description, command: command, schema: schema}
}

func (t *CommandTool) Name() string        { return t.name }
func (t *CommandTool) Description() string { return t.description }

func (t *CommandTool) Parameters() map[string]any { return t.schema }

func (t *CommandTool) Run(ctx *Context) (string, error) {
	if t.command == "" {
		return "", fmt.Errorf("%s: command is empty", t.name)
	}
	argsJSON, err := json.Marshal(ctx.Args)
	if err != nil {
		return "", fmt.Errorf("%s: marshal args: %w", t.name, err)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = DefaultShellTimeout
	}
	base := ctx.Context
	if base == nil {
		base = context.Background()
	}
	cctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, shell, "-c", t.command)
	cmd.Dir = ctx.WorkingDir
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(argsJSON)
	setProcessGroup(cmd)

	// Stream output lines to Notify when set (live output in the TUI), while
	// still collecting the full output for the result.
	out := &bytes.Buffer{}
	if ctx.Notify != nil {
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw

		scannerDone := make(chan struct{})
		go func() {
			defer close(scannerDone)
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				ctx.Notify(sc.Text())
				out.Write(sc.Bytes())
				out.WriteByte('\n')
			}
		}()

		runErr := cmd.Run()
		_ = pw.Close()
		<-scannerDone
		if runErr != nil && !benignPipeClose(runErr) {
			if cctx.Err() != nil {
				return out.String() + fmt.Sprintf("\n[%s timed out]", t.name), nil
			}
			return out.String() + fmt.Sprintf("\n[%s failed: %v]", t.name, runErr), nil
		}
		return strings.TrimRight(out.String(), "\n"), nil
	}

	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil && !benignPipeClose(err) {
		if cctx.Err() != nil {
			return out.String() + fmt.Sprintf("\n[%s timed out]", t.name), nil
		}
		return out.String() + fmt.Sprintf("\n[%s failed: %v]", t.name, err), nil
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
