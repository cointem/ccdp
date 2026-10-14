package hooks

import (
	"context"
	"fmt"
	"testing"
)

func TestShellHookDecisionsDenyOverridesAskAndAllow(t *testing.T) {
	for _, tc := range []struct {
		decisions []Decision
		want      Decision
	}{
		{[]Decision{DecisionAllow, DecisionDeny}, DecisionDeny},
		{[]Decision{DecisionAsk, DecisionDeny}, DecisionDeny},
		{[]Decision{DecisionAsk, DecisionAllow}, DecisionAsk},
		{[]Decision{DecisionAllow, DecisionAsk}, DecisionAsk},
		{[]Decision{DecisionAsk, DecisionBlock}, DecisionBlock},
	} {
		var specs []HookSpec
		for _, decision := range tc.decisions {
			specs = append(specs, HookSpec{Command: fmt.Sprintf(`printf '%%s' '{"decision":"%s","reason":"%s"}'`, decision, decision)})
		}
		manager := NewManager(Config{EventPreToolUse: specs}, testOptions(t.TempDir()))
		out := manager.PreToolUse(context.Background(), "Read", nil)
		if out.Decision != tc.want {
			t.Fatalf("%v produced %+v", tc.decisions, out)
		}
	}
}

func TestContinueFalseIsStickyAcrossHookChain(t *testing.T) {
	manager := NewManager(Config{EventUserPromptSubmit: []HookSpec{
		{Command: `printf '%s' '{"continue":false,"reason":"stop"}'`},
		{Command: `printf '%s' '{"continue":true,"additionalContext":"later"}'`},
	}}, testOptions(t.TempDir()))
	out := manager.UserPromptSubmit(context.Background(), "work")
	if out.Continue == nil || *out.Continue || out.Reason != "stop" || out.AdditionalContext != "later" {
		t.Fatalf("stop verdict discarded: %+v", out)
	}
}

func TestContinueFalseSurvivesLaterDecision(t *testing.T) {
	for _, later := range []string{`exit 2`, `printf '%s' '{"decision":"allow","reason":"allow"}'`, `printf '%s' '{"decision":"ask","reason":"ask"}'`, `printf '%s' '{"decision":"deny","reason":"deny"}'`, `printf invalid`} {
		opts := testOptions(t.TempDir())
		opts.FailClosed = true
		manager := NewManager(Config{EventPreToolUse: []HookSpec{{Command: `printf '%s' '{"continue":false,"reason":"stop"}'`}, {Command: later}}}, opts)
		out := manager.PreToolUse(context.Background(), "Read", nil)
		if out.Continue == nil || *out.Continue || out.Reason != "stop" {
			t.Fatalf("%s discarded stop verdict: %+v", later, out)
		}
	}
}
