package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestRequestSettlementRetryDoesNotChargeOrPublishTwice(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			a, p, _ := newJournalAgent(t, memory)
			attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
			if err != nil {
				t.Fatal(err)
			}
			var updates atomic.Int32
			a.services.evbus.Subscribe(events.TopicUsageUpdated, func(events.Topic, events.Payload) { updates.Add(1) })
			usage := llm.StreamResult{PromptTokens: 8, CompletionTok: 3}
			for range 2 {
				if err := a.finishRequestAttempt(attempt, usage, nil); err != nil {
					t.Fatal(err)
				}
			}
			if got := a.Usage(); got.InputTokens != 8 || got.OutputTokens != 3 || got.TurnCount != 1 {
				t.Fatalf("duplicate usage: %+v", got)
			}
			if updates.Load() != 1 {
				t.Fatalf("published %d times", updates.Load())
			}
			rows, err := p.Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, row := range rows {
				if _, ok := row.Event.(*session.AttemptFinished); ok {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("completion facts = %d", count)
			}
			usage.CompletionTok++
			if err := a.finishRequestAttempt(attempt, usage, nil); err == nil {
				t.Fatal("conflicting completion accepted")
			}
			if got := a.Usage(); got.OutputTokens != 3 {
				t.Fatalf("conflicting completion changed usage: %+v", got)
			}
		})
	}
}

func TestRequestOwnerUsageCallbackCanSubmit(t *testing.T) {
	a := newRuntimeAgent(t)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var callbackErr error
	var callbacks int
	dispose := a.services.evbus.Subscribe(events.TopicUsageUpdated, func(events.Topic, events.Payload) {
		callbacks++
		receipt, err := a.Submit(ctx, protocol.Command{ID: "usage-callback", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandQuery, Query: &protocol.QueryCommand{Kind: protocol.QueryStatus}})
		callbackErr = err
		if err == nil && receipt.Rejected() {
			callbackErr = fmt.Errorf("query rejected: %+v", receipt.Error)
		}
	})
	defer dispose()
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 8, CompletionTok: 3}, nil); err != nil {
		t.Fatal(err)
	}
	if callbackErr != nil || callbacks != 1 {
		t.Fatalf("callbacks=%d error=%v", callbacks, callbackErr)
	}
}

func TestRequestOwnerSettlesConcurrentRetriesAfterCancellation(t *testing.T) {
	a := newRuntimeAgent(t)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	a.lifecycle.rootCancel()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			failures <- a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 8, CompletionTok: 3}, context.Canceled)
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := a.Usage(); got.InputTokens != 8 || got.OutputTokens != 3 || got.TurnCount != 1 {
		t.Fatalf("usage: %+v", got)
	}
}
