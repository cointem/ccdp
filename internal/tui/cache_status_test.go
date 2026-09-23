package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
)

func TestSessionCacheStatus(t *testing.T) {
	cases := []struct {
		cache protocol.CacheStats
		want  string
	}{
		{protocol.CacheStats{}, "cache ?"},
		{protocol.CacheStats{Tracking: true}, "cache —"},
		{protocol.CacheStats{Tracking: true, InputTokens: 100, ReportedInputTokens: 100}, "cache 0%"},
		{protocol.CacheStats{Tracking: true, InputTokens: 1000, ReportedInputTokens: 1000, CachedTokens: 900}, "cache 90%"},
		{protocol.CacheStats{Tracking: true, InputTokens: 1000, ReportedInputTokens: 900, CachedTokens: 900}, "cache ?"},
		{protocol.CacheStats{Tracking: true, UnknownHistory: true, InputTokens: 1000, ReportedInputTokens: 1000, CachedTokens: 900}, "cache ?"},
	}
	for _, c := range cases {
		if got := cacheLabel(c.cache); got != c.want {
			t.Fatalf("%+v: %s want %s", c.cache, got, c.want)
		}
	}
	m := sugModel()
	m.width = 120
	m.hasSnapshot = true
	m.snapshot.Usage.Cache = protocol.CacheStats{Tracking: true, InputTokens: 1000, ReportedInputTokens: 1000, CachedTokens: 900}
	m.snapshot.Usage.InputTokens = 1000
	if !strings.Contains(sanitizeANSI(m.renderPersistentStatus()), "cache 90%") {
		t.Fatal("cache missing beside context")
	}
	if !strings.Contains(sanitizeANSI(m.renderContextDetailPopover()), "900 / 1,000") {
		t.Fatal("detail missing counters")
	}
}
