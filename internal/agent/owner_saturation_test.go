package agent

import (
	"ccdp/internal/protocol"
	"context"
	"fmt"
	"testing"
	"time"
)

func saturatedOwner(t testing.TB) *Agent {
	t.Helper()
	a := acceptanceAgent(t, true)
	a.capabilities.settingsPreparing = true // exclusive construction
	a.ensureRun()
	for i := 0; i < 128; i++ {
		effort := "low"
		a.owner.commands <- runtimeCommand{command: protocol.Command{ID: protocol.CommandID(fmt.Sprintf("queued-%d", i)), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}}, result: make(chan protocol.Receipt, 1)}
	}
	return a
}
func TestOwnerControlBypassesSaturatedOrdinaryQueue(t *testing.T) {
	a := saturatedOwner(t)
	done := make(chan struct{})
	a.owner.control <- turnPhaseChanged{phase: protocol.PhasePreparing, done: done}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated ordinary queue blocked control")
	}
	if len(a.owner.commands) != 64 {
		t.Fatalf("ordinary queue not full: %d", len(a.owner.commands))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	receipt, err := a.Submit(ctx, protocol.Command{ID: "saturated-stop", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandStop})
	if err != nil || receipt.Rejected() {
		t.Fatalf("stop could not bypass ordinary queue: %+v %v", receipt, err)
	}
	if err := a.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
}
func BenchmarkAcceptanceSaturatedControl(b *testing.B) {
	a := saturatedOwner(b)
	samples := make([]int64, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		receipt, err := a.Submit(context.Background(), protocol.Command{ID: protocol.CommandID(fmt.Sprintf("stop-%d", i)), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandStop})
		if err != nil || receipt.Rejected() {
			b.Fatalf("control failed: %+v %v", receipt, err)
		}
		samples[i] = time.Since(start).Nanoseconds()
	}
	b.StopTimer()
	reportLatency(b, samples)
	b.ReportMetric(128, "ordinary-backlog")
}
