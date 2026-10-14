package agent

import (
	"context"
	"math"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestAttemptUsageOverflowDoesNotCommitOrChangeState(t *testing.T) {
	for _, initial := range []Usage{
		{InputTokens: math.MaxInt},
		{OutputTokens: math.MaxInt},
		{TurnCount: math.MaxInt},
		{Cache: protocol.CacheStats{InputTokens: math.MaxInt}},
		{Cache: protocol.CacheStats{ReportedInputTokens: math.MaxInt}},
	} {
		a, p, _ := newJournalAgent(t, true)
		attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
		if err != nil {
			t.Fatal(err)
		}
		a.conversation.durable.Usage = sessionUsage(initial)
		cursor := p.CurrentCursor()
		if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 2, CompletionTok: 1, CacheReported: true}, nil); err == nil {
			t.Fatalf("overflow accepted: %+v", initial)
		}
		if got := a.Usage(); got != initial || p.CurrentCursor() != cursor {
			t.Fatalf("overflow changed state or committed facts: usage=%+v cursor=%d", got, p.CurrentCursor())
		}
	}
}

func TestSharedAttemptUsageOverflowDoesNotCommit(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	a.conversation.durable.Usage = sessionUsage(Usage{InputTokens: math.MaxInt})
	cursor := p.CurrentCursor()
	err := a.accountSharedAttempt(session.Attempt{SessionID: "child", AttemptID: "attempt", Usage: session.Usage{InputTokens: 1, TotalTokens: 1, TurnCount: 1}})
	if err == nil || p.CurrentCursor() != cursor || a.Usage().InputTokens != math.MaxInt {
		t.Fatalf("shared accounting accepted overflow: err=%v usage=%+v", err, a.Usage())
	}
}
