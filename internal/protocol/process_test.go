package protocol

import (
	"strings"
	"testing"
)

func TestProcessBudgetKeepsReferenceAndUnconsumedRawCursor(t *testing.T) {
	p := ProcessOutput{ID: "p.test", Status: "exited", ResultID: "result.p.test", OutputID: strings.Repeat("a", 64), Offset: 100, Next: 1100, Output: strings.Repeat("�", 1000)}
	v, text := FitProcessOutput(p, 350)
	if text == "" || v.Next != v.Offset || v.Output != "" || !v.Truncated || v.OutputID != p.OutputID || !strings.Contains(text, "ReadOutput") {
		t.Fatalf("unsafe budget projection: %+v %s", v, text)
	}
	if _, text := FitProcessOutput(p, 10); text != "" {
		t.Fatal("unrepresentable result marked deliverable")
	}
}

func TestProcessTextUsesOnlyActionableMetadata(t *testing.T) {
	code := 0
	p := ProcessOutput{ID: "p.test", ResultID: "result.p.test", OutputID: strings.Repeat("a", 64), Status: "exited", ExitCode: &code, Output: "ok\n", Next: 3, Log: "/tmp/log"}
	if text := p.Text(); text != "Exit code: 0\nok\n" {
		t.Fatal(text)
	}
	p.Truncated = true
	if text := p.Text(); !strings.Contains(text, `ReadOutput output_id="`+p.OutputID+`" offset=3`) || strings.Contains(text, "result.p.test") || strings.Contains(text, "/tmp/log") {
		t.Fatal(text)
	}
	p.Status, p.ExitCode, p.Truncated = "running", nil, false
	if text := p.Text(); !strings.Contains(text, "process_id=p.test") {
		t.Fatal(text)
	}
}
