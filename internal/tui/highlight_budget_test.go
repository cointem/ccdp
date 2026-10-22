package tui

import (
	"container/list"
	"fmt"
	"strings"
	"testing"
)

func TestHighlightCacheBoundsRetainedBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	highlightMu.Lock()
	oldCache, oldLRU, oldBytes := highlightCache, highlightLRU, highlightCacheBytes
	highlightCache = make(map[highlightKey]*list.Element)
	highlightLRU = list.New()
	highlightCacheBytes = 0
	highlightMu.Unlock()
	t.Cleanup(func() {
		highlightMu.Lock()
		highlightCache, highlightLRU, highlightCacheBytes = oldCache, oldLRU, oldBytes
		highlightMu.Unlock()
	})
	for i := 0; i < 100; i++ {
		source := fmt.Sprintf("// %d %s", i, strings.Repeat("x", 60000))
		if len(highlightCode([]string{source}, "go", true)) != 1 {
			t.Fatal("highlight failed")
		}
	}
	highlightMu.Lock()
	before := highlightLRU.Len()
	bytes := highlightCacheBytes
	highlightMu.Unlock()
	if bytes > maxHighlightCacheBytes || before >= 100 {
		t.Fatalf("cache not bounded: bytes=%d entries=%d", bytes, before)
	}
	large := "// " + strings.Repeat("x", maxHighlightEntryBytes)
	if len(highlightCode([]string{large}, "go", true)) != 1 {
		t.Fatal("large block was not rendered")
	}
	highlightMu.Lock()
	defer highlightMu.Unlock()
	if highlightLRU.Len() != before {
		t.Fatal("large block was retained")
	}
}
