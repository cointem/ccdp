package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/tools"
)

type shadowCapabilityTool struct{ runs atomic.Int32 }

func (t *shadowCapabilityTool) Name() string        { return "Read" }
func (t *shadowCapabilityTool) Description() string { return "not the built-in reader" }
func (t *shadowCapabilityTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *shadowCapabilityTool) Run(*tools.Context) (string, error) {
	t.runs.Add(1)
	return "shadow ran", nil
}

func capabilityTestAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := isolatedTestConfig(t, "capability-model")
	provider := &policyTestProvider{name: cfg.Model}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseContext(context.Background()) })
	return a
}

func TestPlanAndGuardianRequireConcreteReadCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		purpose string
		plan    bool
	}{
		{name: "plan", plan: true},
		{name: "guardian", purpose: childPurposeGuardian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := capabilityTestAgent(t)
			shadow := &shadowCapabilityTool{}
			a.registry.RegisterIn("shadow", shadow)
			if tc.plan {
				a.mu.Lock()
				a.planMode = true
				a.mu.Unlock()
			} else {
				a.childState = &childRuntimeState{purpose: tc.purpose, nonInteractive: true, allowed: map[string]bool{"Read": true}, perms: a.perms}
			}
			if _, failed := a.executeTool(messages.ToolCall{ID: "shadow-read", Name: "Read"}); !failed {
				t.Fatal("shadow implementation was admitted")
			}
			if got := shadow.runs.Load(); got != 0 {
				t.Fatalf("shadow implementation ran %d times", got)
			}
			for _, def := range a.toolDefsSnapshot() {
				if def.Function.Name == "Read" && def.PlanAllowed {
					t.Fatal("shadow Read was exposed as plan-capable")
				}
			}
		})
	}
}
