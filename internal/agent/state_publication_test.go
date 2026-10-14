package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func TestOrdinaryStateOmitsHistoryAndUnchangedCatalog(t *testing.T) {
	a := newRuntimeAgent(t)
	defer a.Close()
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "keep this in full snapshots"}); err != nil {
		t.Fatal(err)
	}
	sub, err := a.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	initial := <-sub.Updates()
	if initial.Snapshot == nil || len(initial.Snapshot.History) != 1 || len(initial.Snapshot.Catalog.Tools) == 0 {
		t.Fatalf("incomplete initial snapshot: %+v", initial)
	}
	a.publishState()
	first := <-sub.Updates()
	if first.State == nil || first.Snapshot != nil || first.State.HistoryLength != 1 {
		t.Fatalf("ordinary state is not lightweight: %+v", first)
	}
	a.publishState()
	second := <-sub.Updates()
	if second.State == nil || second.Snapshot != nil || second.Catalog != nil {
		t.Fatalf("unchanged state resent static data: %+v", second)
	}
	a.mu.Lock()
	a.sequences.settingsRev++
	a.mu.Unlock()
	a.publishState()
	changed := <-sub.Updates()
	if changed.Catalog == nil {
		t.Fatal("settings/catalog changes must replace the catalog explicitly")
	}
}

func TestHistoryTokenCacheTracksAppendRewriteAndBaseline(t *testing.T) {
	a := &Agent{}
	first := messages.Message{Role: messages.RoleUser, Content: strings.Repeat("hello ", 40)}
	second := messages.Message{Role: messages.RoleAssistant, Content: "result"}
	a.conversation.durable.History = make([]messages.Message, 0, 4)
	a.conversation.durable.History = append(a.conversation.durable.History, first)
	want := estimateMessageTokens(first)
	if got := a.contextUsedTokensLocked(); got != want {
		t.Fatalf("first=%d want=%d", got, want)
	}
	a.conversation.durable.History = append(a.conversation.durable.History, second)
	want += estimateMessageTokens(second)
	if got := a.contextUsedTokensLocked(); got != want {
		t.Fatalf("append=%d want=%d", got, want)
	}
	a.turnMemory.tokenBaseline.promptTokens = 500
	a.turnMemory.tokenBaseline.historyLen = 1
	if got := a.contextUsedTokensLocked(); got != 500+estimateMessageTokens(second) {
		t.Fatalf("baseline=%d", got)
	}
	a.conversation.durable.History = []messages.Message{{Role: messages.RoleUser, Content: "replacement"}}
	a.turnMemory.tokenBaseline.promptTokens = 0
	if got := a.contextUsedTokensLocked(); got != estimateMessageTokens(a.conversation.durable.History[0]) {
		t.Fatalf("rewrite=%d", got)
	}
	a.conversation.durable.History = nil
	if got := a.contextUsedTokensLocked(); got != 0 {
		t.Fatalf("clear=%d", got)
	}
}

func BenchmarkOrdinaryStatePublication(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			cfg := config.Default()
			cfg.Workspace = b.TempDir()
			cfg.NoSessionPersistence = true
			cfg.EnableMemory = config.BoolPtr(false)
			a, err := testNewWithOptions(b, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			text := strings.Repeat("history payload ", 200)
			a.mu.Lock()
			for i := 0; i < count; i++ {
				a.conversation.durable.History = append(a.conversation.durable.History, messages.Message{ID: fmt.Sprint(i), Role: messages.RoleAssistant, Content: text})
			}
			a.mu.Unlock()
			// Construction has not started Run, so this benchmark owns the
			// publication path synchronously. Count snapshot construction and
			// Hub delivery, not just the worker's coalesced wakeup enqueue.
			a.publishState()
			a.drainViews() // warm token estimate and static catalog
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				a.publishState()
				a.drainViews()
			}
			b.StopTimer()
		})
	}
}
