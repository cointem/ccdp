package agent

import (
	"context"
	"math"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func TestResumeRejectsInvalidUsage(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	for _, usage := range []session.Usage{
		{InputTokens: -1}, {OutputTokens: -1}, {CachedTokens: -1}, {TurnCount: -1},
	} {
		snapshot := &archive.Snapshot{ID: "invalid-usage", Workspace: cfg.Workspace, Model: cfg.Model, Usage: usage}
		if a, err := Resume(&cfg, snapshot); err == nil {
			a.Close()
			t.Fatalf("resume accepted invalid counters: %+v", usage)
		}
	}
}

func TestAttemptUsageOverflowDoesNotCommitOrChangeState(t *testing.T) {
	for _, initial := range []protocol.UsageSnapshot{
		{InputTokens: math.MaxInt},
		{OutputTokens: math.MaxInt},
		{TurnCount: math.MaxInt},
		{Cache: protocol.CacheStats{InputTokens: math.MaxInt}},
		{Cache: protocol.CacheStats{ReportedInputTokens: math.MaxInt}},
	} {
		a, p, _ := newJournalAgent(t, true)
		attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest(), testRequestMetadata(a))
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
	a.conversation.durable.Usage = sessionUsage(protocol.UsageSnapshot{InputTokens: math.MaxInt})
	cursor := p.CurrentCursor()
	err := a.accountSharedAttempt(session.Attempt{SessionID: "child", AttemptID: "attempt", Usage: session.Usage{InputTokens: 1, TotalTokens: 1, TurnCount: 1}})
	if err == nil || p.CurrentCursor() != cursor || a.Usage().InputTokens != math.MaxInt {
		t.Fatalf("shared accounting accepted overflow: err=%v usage=%+v", err, a.Usage())
	}
}
