package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	m, err := ctx.processManager()
	if err != nil {
		return "", err
	}
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithCancel(call)
	defer cancel()
	id, mp, err := m.startManaged(runCtx, t.command, ctx.WorkingDir, ctx.Sandbox, false, true, timeout)
	if err != nil {
		return "", err
	}
	if err = mp.write(string(argsJSON)); err != nil {
		_, _, _ = mp.stop()
		return "", err
	}
	_ = mp.cmd.CloseInput()
	select {
	case <-mp.done:
	case <-runCtx.Done():
		_, _, _ = mp.stop()
	}
	out, err := mp.view(ctx, id, 0, min(51200, max(4, (ctx.outputLimit()-2048)/6)), 0)
	if err != nil {
		return "", err
	}
	if runCtx.Err() != nil {
		return out + "\n[command timed out or cancelled]", runCtx.Err()
	}
	if mp.cmd.Reason() == "timed_out" {
		return out + "\n[command timed out]", context.DeadlineExceeded
	}
	mp.mu.Lock()
	exitErr := mp.err
	mp.mu.Unlock()
	if exitErr != nil {
		return out + "\n[command failed]", exitErr
	}
	return out, nil
}
