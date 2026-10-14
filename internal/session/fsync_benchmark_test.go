package session

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Includes real JSONL append and file.Sync, with no injected store or mocked I/O.
func BenchmarkAcceptanceFsync(b *testing.B) {
	store, err := OpenJSONLStore(b.TempDir(), "fsync-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Commit(Beginning, Batch{TransactionID: "created", Events: []Event{SessionCreated{SessionID: "fsync-benchmark", FormatVersion: SchemaVersion, Source: "benchmark", CreatedAt: time.Now().UTC()}}}); err != nil {
		b.Fatal(err)
	}
	samples := make([]int64, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := Batch{TransactionID: fmt.Sprintf("memory-%d", i), Events: []Event{MemoryChanged{Revision: uint64(i + 1), Text: "benchmark durable state"}}}
		start := time.Now()
		_, err = store.Commit(store.CurrentCursor(), batch)
		samples[i] = time.Since(start).Nanoseconds()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/1000, "p95-us")
		b.ReportMetric(float64(samples[(len(samples)-1)*99/100])/1000, "p99-us")
	}
}
