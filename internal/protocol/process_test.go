package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessBudgetKeepsReferenceAndUnconsumedRawCursor(t *testing.T) {
	p := ProcessOutput{ID: "p.test", Status: "exited", ResultID: "result.p.test", OutputID: strings.Repeat("a", 64), Offset: 100, Next: 1100, Output: strings.Repeat("�", 1000)}
	v, text := FitProcessOutput(p, 350)
	if text == "" || v.Next != v.Offset || v.Output != "" || !v.Truncated || v.OutputID != p.OutputID || !json.Valid([]byte(text)) {
		t.Fatalf("unsafe budget projection: %+v %s", v, text)
	}
	if _, text := FitProcessOutput(p, 10); text != "" {
		t.Fatal("unrepresentable result marked deliverable")
	}
}
