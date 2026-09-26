package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"ccdp/internal/execution"
)

// CommandTool is a user-defined external tool (Codex's config tools). When
// the model calls it, the configured shell command runs with tool arguments
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

func (t *CommandTool) Parameters() map[string]any { return WithCapabilityRequest(t.schema) }

func (t *CommandTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	if strings.TrimSpace(t.command) == "" {
		return "", fmt.Errorf("%s: command is empty", t.name)
	}
	argsJSON, err := json.Marshal(ctx.Args)
	if err != nil {
		return "", fmt.Errorf("%s: marshal args: %w", t.name, err)
	}
	res, err := execution.Run(execution.Request{
		Context:       ctx.Context,
		Command:       t.command,
		Dir:           ctx.WorkingDir,
		Timeout:       ctx.Timeout,
		Input:         bytes.NewReader(argsJSON),
		Sandbox:       ctx.Sandbox,
		NotifyContext: ctx.notifyContext(),
		OutputLimit:   ctx.outputLimit(),
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	out := strings.TrimRight(res.Output, "\n")
	if res.TimedOut {
		out += fmt.Sprintf("\n[%s timed out]", t.name)
	} else if res.ExitCode != 0 {
		out += fmt.Sprintf("\n[%s failed: exit code %d]", t.name, res.ExitCode)
	}
	return boundedToolString(ctx, out), nil
}
