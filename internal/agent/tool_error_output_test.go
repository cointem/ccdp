package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/tools"
	"errors"
	"strings"
	"testing"
)

type diagnosticFailureTool struct{}

func (diagnosticFailureTool) Name() string               { return "DiagnosticFailure" }
func (diagnosticFailureTool) Description() string        { return "test" }
func (diagnosticFailureTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (diagnosticFailureTool) Run(*tools.Context) (string, error) {
	return "fatal: not a git repository\n", errors.New("exit 128")
}

func TestToolFailurePreservesDiagnosticOutput(t *testing.T) {
	a := capabilityTestAgent(t)
	output, failed := a.runToolWithJournal(messages.ToolCall{ID: "failure", Name: "DiagnosticFailure"}, diagnosticFailureTool{}, toolJournalContext{}, nil)
	if !failed || !strings.Contains(output, "exit 128") || !strings.Contains(output, "fatal: not a git repository") {
		t.Fatalf("lost diagnostics: %q", output)
	}
}
