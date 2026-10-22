package agent

import (
	"context"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
)

func TestInvalidChildConcurrencyReturnsConstructionError(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = t.TempDir()
	cfg.MaxConcurrentAgents = 0
	a, err := New(&cfg)
	if a != nil {
		a.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "max_concurrent_agents") {
		t.Fatalf("want validation error, got %v", err)
	}
	t.Logf("public constructor returned error without panic: %v", err)
}

func TestDeliveredContinuationRetryReturnsDurableReceipt(t *testing.T) {
	p := &childRuntimeTestProvider{name: "review-probe", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "ok", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.services.supervisor.launch(a, a.lifecycle.rootCtx, childTask{Description: "original"}, childPurposeTask, "probe-call")
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	id := r.fact.Child.SessionID
	reader, err := a.Sessions().OpenReader(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	cmd := protocol.NewSubmitInput("probe-continuation", id, "probe-input", "next", protocol.InputSteer)
	first, err := reader.Submit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.services.supervisor.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, next)
	retry, err := reader.Submit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	canonical, found, err := a.services.supervisor.priorChildInputReceipt(context.Background(), next, cmd)
	if err != nil || !found {
		t.Fatalf("missing durable input: %v", err)
	}
	t.Logf("first=%+v retry=%+v durable=%+v", first, retry, canonical)
	if retry.Status != protocol.ReceiptApplied || retry.Revision != canonical.Revision || canonical.Revision.LogSeq == 0 {
		t.Fatal("unexpected receipt behavior")
	}
}
