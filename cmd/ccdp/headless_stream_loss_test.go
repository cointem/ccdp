package main

import (
	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type streamLossProvider struct {
	text  string
	chunk int
}

func (*streamLossProvider) Name() string { return "audit-large" }
func (p *streamLossProvider) Stream(ctx context.Context, r llm.CompletionRequest, delta func(string)) (llm.StreamResult, error) {
	for start := 0; start < len(p.text); start += p.chunk {
		end := min(len(p.text), start+p.chunk)
		delta(p.text[start:end])
	}
	return llm.StreamResult{Text: p.text, FinishReason: "stop", PromptTokens: 1, CompletionTok: 1}, nil
}
func TestHeadlessStreamQueueLossRecovery(t *testing.T) {
	for _, format := range []outputFormat{outText, outJSON, outStreamJSON} {
		for _, chunk := range []int{1024, 400000} {
			t.Run(fmt.Sprintf("%s/chunk-%d", format, chunk), func(t *testing.T) {
				size := 300000
				cfg := config.Default()
				cfg.Workspace = t.TempDir()
				cfg.NoSessionPersistence = true
				cfg.EnableProjectMemory = false
				cfg.Model = "audit-model"
				p := &streamLossProvider{chunk: chunk, text: "MISSING-PREFIX-" + strings.Repeat("x", size)}
				r := plugin.NewModelRegistry()
				r.Register(p)
				r.Route(cfg.Model, p.Name())
				a, err := agent.NewWithOptions(&cfg, agent.Options{EffectiveConfigFrozen: true, ProviderRegistry: r})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				w := &headlessWatch{client: a}
				if err := w.open(ctx); err != nil {
					t.Fatal(err)
				}
				defer w.close()
				if _, err := w.next(ctx); err != nil {
					t.Fatal(err)
				}
				cmd := protocol.NewSubmitInput("audit-cmd", w.snapshot.SessionID, "audit-input", "respond", protocol.InputSteer)
				receipt, err := a.Submit(ctx, cmd)
				if err != nil || receipt.Rejected() {
					t.Fatalf("submit=%+v err=%v", receipt, err)
				}
				s := turnSink{historyAnchor: protocol.InputMessageID("audit-input")}
				err = consumeHeadlessTurn(ctx, ctx, w, w.snapshot.SessionID, format, &s, 0, a)
				if format == outStreamJSON {
					// A slow/race-instrumented run may flush between the
					// small callbacks. Success is valid only if none was lost.
					if chunk == 1024 && err == nil && s.text.String() == p.text {
						return
					}
					if err == nil || !strings.Contains(err.Error(), "resynchronized") {
						t.Fatalf("lost stream reported success: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				final, err := a.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				durable := latestAssistant(final.History, 0)
				if durable != p.text {
					t.Fatalf("fixture did not commit full response: %d", len(durable))
				}
				if s.text.String() != p.text {
					t.Fatalf("success lost text: got=%d want=%d prefix=%t resync=%v", s.text.Len(), len(p.text), strings.HasPrefix(s.text.String(), "MISSING-PREFIX-"), s.resynchronized)
				}
			})
		}
	}
}
