package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

func TestCompactionRequiresCompleteSummary(t *testing.T) {
	for _, finish := range []string{"length", "content_filter", "error", "tool_calls", "", "stop"} {
		t.Run(finish, func(t *testing.T) {
			p := &childRuntimeTestProvider{name: "summary-completion", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				return llm.StreamResult{Text: "checkpoint", FinishReason: finish}, nil
			}}
			a := newJournalRuntimeAgent(t, p)
			a.services.cfg.KeepAfterCompact = 4
			for i := 0; i < 7; i++ {
				if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: strings.Repeat("critical instructions ", 100)}); err != nil {
					t.Fatal(err)
				}
			}
			before := a.History()
			err := a.compactContext(context.Background())
			if finish == "stop" {
				if err != nil {
					t.Fatal(err)
				}
				if reflect.DeepEqual(before, a.History()) {
					t.Fatal("complete summary not applied")
				}
				return
			}
			if err == nil {
				t.Fatal("incomplete summary accepted")
			}
			if !reflect.DeepEqual(before, a.History()) {
				t.Fatal("incomplete summary replaced history")
			}
		})
	}
}
