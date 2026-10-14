package plugin

import "testing"

func TestGoHookDecisionsDenyOverridesAskAndAllow(t *testing.T) {
	for _, decisions := range [][]ToolDecision{
		{DecisionAllow, DecisionDeny}, {DecisionAsk, DecisionDeny},
		{DecisionAllow, DecisionAsk, DecisionDeny}, {DecisionAsk, DecisionAllow, DecisionDeny},
	} {
		hooks := NewGoHooks()
		for _, decision := range decisions {
			hooks.AddPreTool(func(string, map[string]any) (ToolDecision, string) { return decision, "policy" })
		}
		if got, _ := hooks.RunPreTool("anything", nil); got != DecisionDeny {
			t.Fatalf("%v produced %v", decisions, got)
		}
	}
	for _, decisions := range [][]ToolDecision{{DecisionAsk, DecisionAllow}, {DecisionAllow, DecisionAsk}} {
		hooks := NewGoHooks()
		calls := 0
		for _, decision := range decisions {
			hooks.AddPreTool(func(string, map[string]any) (ToolDecision, string) { calls++; return decision, "policy" })
		}
		if got, _ := hooks.RunPreTool("anything", nil); got != DecisionAsk || calls != 2 {
			t.Fatalf("%v produced %v after %d hooks", decisions, got, calls)
		}
	}
}
