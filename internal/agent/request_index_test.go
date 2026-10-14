package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/session"
)

type rejectRequestReadStore struct{ session.Store }

func (s rejectRequestReadStore) Read(session.Cursor) ([]session.Record, error) {
	return nil, errors.New("settlement must not scan history")
}

func TestRequestSettlementUsesCommittedIndexWithoutReadingHistoryOrBlob(t *testing.T) {
	a, p, cfg := newJournalAgent(t, false)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.SessionDir, a.SessionID(), "blobs", attempt.Manifest.WireBody.Hash)); err != nil {
		t.Fatal(err)
	}
	attempt.Manifest.Model = "caller-tampered"
	attempt.Manifest.Purpose = "caller-tampered"
	backend := journalBackend(t, p)
	backend.mu.Lock()
	underlying := backend.store
	backend.store = rejectRequestReadStore{underlying}
	backend.mu.Unlock()
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 8, CompletionTok: 3}, nil); err != nil {
		t.Fatal(err)
	}
	records, err := underlying.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if event, ok := record.Event.(*session.AttemptFinished); ok {
			if event.Attempt.Model != "journal-model" || event.Attempt.Purpose != "main" {
				t.Fatalf("caller rewrote committed identity: %+v", event.Attempt)
			}
			return
		}
	}
	t.Fatal("attempt completion missing")
}

func TestCommittedRequestIndexRebuiltOnReopen(t *testing.T) {
	a, p, cfg := newJournalAgent(t, false)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	backend := &journalTestBackend{}
	reopened, err := openSessionPersistenceModeWithParent(cfg, a.SessionID(), a.conversation.createdAt, "agent", false, "", backend.options())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	backend.mu.Lock()
	backend.store = rejectRequestReadStore{backend.store}
	backend.mu.Unlock()
	manifest, err := reopened.CommittedRequestManifest(attempt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Model != attempt.Manifest.Model || manifest.SessionID != a.SessionID() {
		t.Fatalf("reopened metadata = %+v", manifest)
	}
}

// Run with -bench=BenchmarkRequestSettlement -benchtime=100x. Existing request
// history should not multiply completion-time allocations or log traversals.
func BenchmarkRequestSettlement(b *testing.B) {
	for _, history := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("history_%d", history), func(b *testing.B) {
			a, p, _ := newJournalAgent(b, true)
			for start := 0; start < history; start += 128 {
				var facts []session.Event
				for i := start; i < min(start+128, history); i++ {
					facts = append(facts, session.RequestPrepared{Manifest: session.RequestManifest{RequestID: fmt.Sprintf("prior-%d", i), SessionID: a.SessionID(), Model: "m", Provider: "p", AdapterVersion: "1", Digest: "d", Messages: []session.Message{{MessageID: fmt.Sprint(i), Role: "user", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "old prompt"}}}}}})
				}
				if _, err := p.Commit(session.Batch{TransactionID: fmt.Sprintf("prior-batch-%d", start), Events: facts}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 8, CompletionTok: 3}, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestMemoryContinuationRebuildsRequestSettlementIndex(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := p.RequestArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreMemoryRun(records, blobs); err != nil {
		t.Fatal(err)
	}
	manifest, err := p.CommittedRequestManifest(attempt.RequestID)
	if err != nil || manifest.Model != "journal-model" {
		t.Fatalf("restored manifest=%+v err=%v", manifest, err)
	}
	if p.RequestAdmitted(attempt.RequestID) {
		t.Fatal("restored request treated as a live admission")
	}
}
