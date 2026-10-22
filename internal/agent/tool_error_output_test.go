package agent

import (
	"errors"
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/tools"
)

type diagnosticFailureTool struct{}

func (diagnosticFailureTool) Name() string { return "DiagnosticFailure" }

func (diagnosticFailureTool) Description() string { return "test" }

func (diagnosticFailureTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (diagnosticFailureTool) Execute(ctx *tools.Context, args map[string]any) (tools.Result, error) {
	ctx = ctx.WithArgs(args)
	text, err := func() (string, error) {
		return "fatal: not a git repository\n", errors.New("exit 128")
	}()
	return tools.Result{Text: text}, err
}

func TestToolFailurePreservesDiagnosticOutput(t *testing.T) {
	a := capabilityTestAgent(t)
	a.services.registry.Register(diagnosticFailureTool{})
	a.services.perms.SetMode(permissions.ModeBypass)
	output, failed := executeTestTool(t, a, messages.ToolCall{ID: "failure", Name: "DiagnosticFailure"})
	if !failed || !strings.Contains(output, "exit 128") || !strings.Contains(output, "fatal: not a git repository") {
		t.Fatalf("lost diagnostics: %q", output)
	}
}
