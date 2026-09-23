package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"ccdp/internal/protocol"
)

func newContextStatusModel(t *testing.T, used, window int) *Model {
	t.Helper()
	m := NewWithClient(nil, "", false)
	t.Cleanup(m.watchCancel)
	m.width = 80
	m.height = 24
	m.statusItems = []string{"context"}
	m.hasSnapshot = true
	m.snapshot = protocol.SessionView{
		Settings:          protocol.SettingsSnapshot{ContextWindow: window},
		ContextUsedTokens: used,
	}
	return &m
}

func TestContextStatusShowsUsedAndCapacity(t *testing.T) {
	tests := []struct {
		name       string
		used       int
		window     int
		want       string
		wantHidden bool
	}{
		{name: "normal", used: 12345, window: 200000, want: "12.3k/200k"},
		{name: "zero", used: 0, window: 200000, want: "0/200k"},
		{name: "under one percent", used: 999, window: 200000, want: "999/200k"},
		{name: "negative used clamps", used: -20, window: 200000, want: "0/200k"},
		{name: "over capacity", used: 250000, window: 200000, want: "250k/200k"},
		{name: "zero capacity", used: 12345, window: 0, wantHidden: true},
		{name: "negative capacity", used: 12345, window: -1, wantHidden: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeANSI(newContextStatusModel(t, tt.used, tt.window).renderHeader())
			if tt.wantHidden {
				if strings.TrimSpace(got) != "" {
					t.Fatalf("header=%q, want context hidden", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("header=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestContextStatusPrefersSnapshotOverUsage(t *testing.T) {
	m := newContextStatusModel(t, 42, 100000)
	m.usage = protocol.UsageSnapshot{InputTokens: 190000, OutputTokens: 9000}
	got := sanitizeANSI(m.renderHeader())
	if !strings.Contains(got, "42/100k") {
		t.Fatalf("header=%q, want snapshot context estimate", got)
	}
	if strings.Contains(got, "ctx:199k") {
		t.Fatalf("header=%q used cumulative usage instead of snapshot", got)
	}
}

func TestContextStatusWarnsAtEightyPercent(t *testing.T) {
	original := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(original) })

	m := newContextStatusModel(t, 160000, 200000)
	want := fitLines(styleHeader.Render(styleContextWarn.Render("160k/200k")), m.width)
	if got := m.renderHeader(); got != want {
		t.Fatalf("warning header=%q, want styled warning=%q", got, want)
	}
}

func TestContextStatusFitsNarrowHeader(t *testing.T) {
	m := newContextStatusModel(t, 12345, 200000)
	m.width = 12
	got := m.renderHeader()
	if width := lipgloss.Width(got); width > m.width {
		t.Fatalf("header width=%d, want <=%d: %q", width, m.width, got)
	}
}

func TestFormatContextTokens(t *testing.T) {
	for _, tt := range []struct {
		value int
		want  string
	}{
		{value: -1, want: "0"},
		{value: 0, want: "0"},
		{value: 999, want: "999"},
		{value: 1000, want: "1k"},
		{value: 1234, want: "1.2k"},
		{value: 12000, want: "12k"},
	} {
		if got := formatContextTokens(tt.value); got != tt.want {
			t.Errorf("formatContextTokens(%d)=%q, want %q", tt.value, got, tt.want)
		}
	}
}
