package tui

import (
	"fmt"
	"testing"
)

func BenchmarkCellReplacement(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var store CellStore
			for i := 0; i < count; i++ {
				store.confirmedItems = append(store.confirmedItems, historyCell{kind: "assistant", messageID: fmt.Sprint(i), text: "old"})
			}
			store.compose()
			cell := store.confirmedItems[count-1]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%2 == 0 {
					cell.text = "new"
				} else {
					cell.text = "old"
				}
				if !store.replace(cell) {
					b.Fatal("missing cell")
				}
			}
		})
	}
}

func BenchmarkHistoryDraft(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var ledger historyLedger
			ledger.ensure()
			for i := 0; i < count; i++ {
				ledger.printed[fmt.Sprint(i)] = struct{}{}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				candidate := ledger.clone()
				ledger.accept(candidate)
			}
		})
	}
}
