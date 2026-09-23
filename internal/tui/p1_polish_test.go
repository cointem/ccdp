package tui

import (
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestCompactWarning(t *testing.T) {
	tests := []struct {
		name      string
		used      int
		window    int
		threshold float64
		wantLabel string
		wantOver  bool
	}{
		{name: "no window", used: 100, window: 0, threshold: 0.8},
		{name: "no threshold", used: 100, window: 1000, threshold: 0},
		{name: "below band", used: 5000, window: 10000, threshold: 0.8, wantLabel: ""},
		{name: "approaching", used: 7500, window: 10000, threshold: 0.8, wantLabel: "⚠ 5% to auto-compact"},
		{name: "over threshold", used: 8200, window: 10000, threshold: 0.8, wantLabel: "⚠ auto-compact now", wantOver: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, over := compactWarning(tt.used, tt.window, tt.threshold)
			if label != tt.wantLabel || over != tt.wantOver {
				t.Fatalf("compactWarning(%d,%d,%v) = (%q,%v), want (%q,%v)", tt.used, tt.window, tt.threshold, label, over, tt.wantLabel, tt.wantOver)
			}
		})
	}
}

func newContextThresholdModel(t *testing.T, used, window int, threshold float64) *Model {
	t.Helper()
	m := NewWithClient(nil, "", false)
	t.Cleanup(m.watchCancel)
	m.width = 80
	m.height = 24
	m.hasSnapshot = true
	m.snapshot = protocol.SessionView{
		Settings: protocol.SettingsSnapshot{
			Model:            protocol.ModelBinding{Model: "deepseek-chat"},
			ReasoningEffort:  "high",
			ContextWindow:    window,
			CompactThreshold: threshold,
		},
		ContextUsedTokens: used,
	}
	return &m
}

// TestCompactWarningKeepsConfirmedFooter guards that the new proximity warning is
// additive: it shows alongside, and never replaces, the confirmed model/effort/
// context triplet the acceptance tests lock in.
func TestCompactWarningKeepsConfirmedFooter(t *testing.T) {
	m := newContextThresholdModel(t, 190000, 200000, 0.95)
	plain := sanitizeANSI(m.renderPersistentStatus())
	for _, want := range []string{"deepseek-chat", "high", "200k", "auto-compact"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("persistent status = %q, missing %q", plain, want)
		}
	}
}

func TestTurnStalled(t *testing.T) {
	fresh := &Model{}
	fresh.busy = true
	fresh.activity.UpdatedAt = time.Now()
	if fresh.turnStalled() {
		t.Fatal("fresh activity reported stalled")
	}
	stale := &Model{}
	stale.busy = true
	stale.activity.Phase = ActivityWaitingResponse
	stale.activity.UpdatedAt = time.Now().Add(-45 * time.Second)
	if !stale.turnStalled() {
		t.Fatal("45s model silence should show a waiting hint")
	}
	idle := &Model{}
	idle.busy = false
	idle.activity.UpdatedAt = time.Now().Add(-10 * time.Second)
	if idle.turnStalled() {
		t.Fatal("not busy should never report stalled")
	}
	zero := &Model{}
	zero.busy = true
	if zero.turnStalled() {
		t.Fatal("no activity timestamp should not report stalled before first event")
	}
}
