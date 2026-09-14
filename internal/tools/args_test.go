package tools

import (
	"encoding/json"
	"math"
	"testing"
)

func TestIntArgCheckedUseNumberAndBounds(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
		ok    bool
	}{
		{name: "json number", value: json.Number("42"), want: 42, ok: true},
		{name: "json exponent integer", value: json.Number("4.2e1"), want: 42, ok: true},
		{name: "fraction", value: json.Number("4.2"), want: 7},
		{name: "overflow", value: json.Number("999999999999999999999999999"), want: 7},
		{name: "float fraction", value: 4.2, want: 7},
		{name: "nonfinite", value: math.Inf(1), want: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IntArgChecked(map[string]any{"n": tc.value}, "n", 7)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("IntArgChecked = %d, %v; want %d, nil", got, err, tc.want)
				}
				return
			}
			if err == nil || got != tc.want {
				t.Fatalf("IntArgChecked = %d, %v; want fallback %d and error", got, err, tc.want)
			}
		})
	}
}

func TestIntArgCheckedMissingUsesFallback(t *testing.T) {
	got, err := IntArgChecked(nil, "n", 9)
	if err != nil || got != 9 {
		t.Fatalf("missing IntArgChecked = %d, %v; want 9, nil", got, err)
	}
}
