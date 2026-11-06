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
func (t *catalogRuntimeTool) Execute(ctx *tools.Context, args map[string]any) (tools.Result, error) {
	ctx = ctx.WithArgs(args)
	text, err := func() (string, error) { return t.description, nil }()
	return tools.Result{Text: text}, err
}

func TestStepToolUpdatesPersistAndFreeze(t *testing.T) {
	a := newRuntimeAgent(t)
	for _, removed := range []string{"EnterPlanMode", "ExitPlanMode"} {
		if _, ok := a.services.registry.Get(removed); ok {
			t.Fatalf("removed tool %s remains in the model catalog", removed)
		}
	}
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "inspect"}); err != nil {
		t.Fatal(err)
	}
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	a.releaseStepContext(step)
	if lastToolUpdate(step.history) == nil || !lastToolUpdate(step.history).Initial {
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
	update := lastToolUpdate(step.history)
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
	update = lastToolUpdate(step.history)
	if update == nil || len(update.Removed) != 1 || len(update.Added) != 1 || update.Added[0].Description != "second" {
		t.Fatalf("bad redefinition: %+v", update)
	}
	a.releaseStepContext(step)
	a.services.registry.UnregisterScope("catalog-test")
	step, err = a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	update = lastToolUpdate(step.history)
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

func TestStepToolCatalogRestartAfterCompaction(t *testing.T) {
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
	if !lastToolUpdate(step.history).Initial {
		t.Fatal("no replacement baseline after compaction")
	}
	a.releaseStepContext(step)
}

func lastToolUpdate(history []messages.Message) *messages.ToolUpdate {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].ToolUpdate != nil {
			return history[i].ToolUpdate
		}
	}
	return nil
}
