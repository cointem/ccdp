package agent

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func acceptanceAgent(b testing.TB, memory bool) *Agent {
	b.Helper()
	cfg := config.Default()
	cfg.Workspace = b.TempDir()
	cfg.SessionDir = cfg.Workspace + "/sessions"
	cfg.NoSessionPersistence = memory
	cfg.EnableMemory = config.BoolPtr(false)
	a, err := NewWithOptions(&cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(a.Close)
	return a
}
func reportLatency(b *testing.B, samples []int64) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/1000, "p95-us")
		b.ReportMetric(float64(samples[(len(samples)-1)*99/100])/1000, "p99-us")
	}
}
func BenchmarkAcceptanceCommand(b *testing.B) {
	for _, memory := range []bool{true, false} {
		b.Run(fmt.Sprintf("memory_%t", memory), func(b *testing.B) {
			a := acceptanceAgent(b, memory)
			go a.Run()
			samples := make([]int64, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				effort := "low"
				if i%2 == 1 {
					effort = "high"
				}
				cmd := protocol.Command{ID: protocol.CommandID(fmt.Sprintf("bench-%d", i)), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}}
				start := time.Now()
				receipt, err := a.Submit(context.Background(), cmd)
				samples[i] = time.Since(start).Nanoseconds()
				if err != nil || receipt.Rejected() {
					b.Fatalf("admission: %+v %v", receipt, err)
				}
			}
			b.StopTimer()
			reportLatency(b, samples)
		})
	}
}

type acceptanceCatalogTool struct{ name string }

func (t acceptanceCatalogTool) Name() string        { return t.name }
func (t acceptanceCatalogTool) Description() string { return "benchmark tool" }
func (t acceptanceCatalogTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
}
func (acceptanceCatalogTool) Run(*tools.Context) (string, error) { return "", nil }
func BenchmarkAcceptanceCatalogRefresh(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			a := acceptanceAgent(b, true)
			// Construction still owns the unpublished registry and catalog.
			for i := 0; i < count; i++ {
				a.services.registry.Register(acceptanceCatalogTool{fmt.Sprintf("catalog_%04d", i)})
			}
			samples := make([]int64, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				a.mu.Lock()
				catalog := a.catalogSnapshotLocked()
				a.mu.Unlock()
				samples[i] = time.Since(start).Nanoseconds()
				if len(catalog.Tools) < count {
					b.Fatal("incomplete catalog")
				}
			}
			b.StopTimer()
			reportLatency(b, samples)
		})
	}
}
