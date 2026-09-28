package protocol

import (
	"strings"
	"testing"
)

func TestReviewMessageValidation(t *testing.T) {
	for _, message := range []string{"a\x00b", strings.Repeat("x", maxCommandSummary+1)} {
		if _, err := (WorkflowCommand{Kind: WorkflowReview, Message: message}).normalize(); err == nil {
			t.Fatal("invalid review message accepted")
		}
	}
	w, err := (WorkflowCommand{Kind: WorkflowReview, Message: "  inspect reads  "}).normalize()
	if err != nil || w.Message != "inspect reads" {
		t.Fatal(w, err)
	}
}
