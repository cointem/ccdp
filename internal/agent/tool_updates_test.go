package agent

import (
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/replay"
	"ccdp/internal/tools"
)

type catalogRuntimeTool struct{ description string }

func (t *catalogRuntimeTool) Name() string        { return "CatalogTest" }
func (t *catalogRuntimeTool) Description() string { return t.description }
func (t *catalogRuntimeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *catalogRuntimeTool) Run(*tools.Context) (string, error) { return t.description, nil }

func TestStepToolUpdatesPersistAndFreeze(t *testing.T) {
	a := newRuntimeAgent(t)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "inspect"}); err != nil {
		t.Fatal(err)
	}
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	a.releaseStepContext(step)
	if step.history[len(step.history)-1].ToolUpdate == nil || !step.history[len(step.history)-1].ToolUpdate.Initial {
		t.Fatal("initial catalog missing")
	}
	count := len(a.History())
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	a.releaseStepContext(step)
	if len(a.History()) != count {
		t.Fatal("unchanged catalog duplicated")
	}

	a.services.registry.RegisterIn("catalog-test", &catalogRuntimeTool{description: "first"})
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	before := a.buildRequestForStep(step)
	update := step.history[len(step.history)-1].ToolUpdate
	if update == nil || len(update.Added) != 1 || update.Added[0].Name != "CatalogTest" {
		t.Fatalf("missing addition: %+v", update)
	}
	a.services.registry.RegisterIn("catalog-test", &catalogRuntimeTool{description: "second"})
	if !reflect.DeepEqual(before, a.buildRequestForStep(step)) {
		t.Fatal("prepared request changed with registry")
	}
	bound, ok := step.toolLease.Get("CatalogTest")
	if !ok || bound.Description() != "first" {
		t.Fatal("execution binding was replaced")
	}
	a.releaseStepContext(step)
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	update = step.history[len(step.history)-1].ToolUpdate
	if update == nil || len(update.Removed) != 1 || len(update.Added) != 1 || update.Added[0].Description != "second" {
		t.Fatalf("bad redefinition: %+v", update)
	}
	a.releaseStepContext(step)
	a.services.registry.UnregisterScope("catalog-test")
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	update = step.history[len(step.history)-1].ToolUpdate
	if update == nil || len(update.Removed) != 1 || len(update.Added) != 0 {
		t.Fatalf("bad removal: %+v", update)
	}
	if strings.Contains(joinSystemBlocks(step.systemBlocks), "# Available tools") {
		t.Fatal("mutable catalog remained in base system")
	}
	a.releaseStepContext(step)

	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := replay.RecordsRaw(records)
	if err != nil {
		t.Fatal(err)
	}
	want, known := messages.CurrentTools(a.History())
	got, restoredKnown := messages.CurrentTools(restored.History)
	if !known || !restoredKnown || messages.DiffTools(want, got, true) != nil {
		t.Fatal("catalog lost during durable replay")
	}
}

func TestStepToolCatalogRestartAfterCompactionAndPlanChange(t *testing.T) {
	a := newRuntimeAgent(t)
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	a.releaseStepContext(step)
	// A compacted tail may keep deltas while dropping their initial baseline.
	if err := a.persistHistory([]messages.Message{{Role: messages.RoleUser, Content: "summary"}, {Role: messages.RoleSystem, ToolUpdate: &messages.ToolUpdate{Removed: []string{"old"}}}}, "turn-1"); err != nil {
		t.Fatal(err)
	}
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	if !step.history[len(step.history)-1].ToolUpdate.Initial {
		t.Fatal("no replacement baseline after compaction")
	}
	a.releaseStepContext(step)
	a.mu.Lock()
	a.plan.planMode = true
	a.mu.Unlock()
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseStepContext(step)
	current, _ := messages.CurrentTools(step.history)
	req := a.buildRequestForStep(step)
	if len(current) != len(req.Tools) {
		t.Fatalf("plan catalog has %d tools, request has %d", len(current), len(req.Tools))
	}
	for _, def := range req.Tools {
		if !def.PlanAllowed {
			t.Fatal("unsafe tool advertised in plan")
		}
	}
}
