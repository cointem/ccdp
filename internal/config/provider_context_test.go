package config

import (
	"encoding/json"
	"testing"
)

func TestProviderContextWindow(t *testing.T) {
	c := Default()
	if err := json.Unmarshal([]byte(`{"model":"other/step-5-preview","providers":{"other":{"base_url":"https://example.com/v1","wire_api":"chat","models":{"step-5-preview":{"context_window":1000000}}}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.ContextWindowFor(c.Model); got != 1000000 {
		t.Fatal(got)
	}
	if got := c.ContextWindowFor("unlisted"); got != c.ContextWindow {
		t.Fatal(got)
	}
	for _, n := range []int{-1, 3999} {
		c.Providers["other"].ModelConfigs["step-5-preview"] = ModelConfig{ContextWindow: n}
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
}

func TestMultipleModelsContextWindows(t *testing.T) {
	c := Default()
	c.Providers = map[string]ProviderConfig{"router": {
		Models: []string{"small", "large", "inherited"}, ContextWindow: 300000,
		ContextWindows: map[string]int{"small": 128000, "large": 1000000},
	}}
	for model, want := range map[string]int{"small": 128000, "large": 1000000, "inherited": 300000, "other": 200000} {
		if got := c.ContextWindowFor(model); got != want {
			t.Fatalf("%s: %d != %d", model, got, want)
		}
	}
	cloned := c.Clone()
	cloned.Providers["router"].ContextWindows["large"] = 400000
	if c.ContextWindowFor("large") != 1000000 {
		t.Fatal("clone shares mutable limits")
	}
	c.Providers["router"].ContextWindows["typo"] = 1000000
	if err := c.Validate(); err == nil {
		t.Fatal("accepted unlisted model")
	}
	delete(c.Providers["router"].ContextWindows, "typo")
	c.Providers["router"].ContextWindows["small"] = 0
	if err := c.Validate(); err == nil {
		t.Fatal("accepted invalid model window")
	}
}
