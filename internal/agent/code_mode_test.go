package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/codemode"
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/replay"
	"ccdp/internal/tools"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == codemode.WorkerArgument {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func codeAgent(t *testing.T, code string, configure func(*config.Config)) (*Agent, *atomic.Int32, <-chan protocol.EventView) {
	t.Helper()
	if !codemode.Available {
		t.Skip("requires Darwin Seatbelt")
	}
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = t.TempDir()
	cfg.MaxParallelTools = 1
	cfg.MaxResultSizeBytes = 8192
	cfg.PermissionMode = string(permissions.ModeAuto)
	if configure != nil {
		configure(&cfg)
	}
	var calls atomic.Int32
	p := &childRuntimeTestProvider{name: "code-test", stream: func(ctx context.Context, req llm.CompletionRequest, onText func(string)) (llm.StreamResult, error) {
		if calls.Add(1) > 1 {
			return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
		}
		args, _ := json.Marshal(map[string]any{"code": code})
		return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "code-parent", Type: "function", Function: llm.Function{Name: "RunCode", Arguments: llm.ArgumentsJSON(args)}}}}, nil
	}}
	models := plugin.NewModelRegistry()
	models.Register(p)
	models.Route(cfg.Model, p.Name())
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, &calls, watchTestEvents(t, a)
}
func finishCode(t *testing.T, a *Agent, events <-chan protocol.EventView) {
	t.Helper()
	submitTestInput(t, a, "execute code")
	drainUntil(t, events, 20*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
}

func TestCodeModeManualAndReload(t *testing.T) {
	a := newRuntimeAgent(t)
	if _, ok := a.services.registry.Get("RunCode"); ok != codemode.Available {
		t.Fatal("RunCode must be registered by default")
	}
	dir := config.ProjectSettingsDir(a.services.cfg.Workspace)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		contents string
		enabled  bool
	}{
		{contents: `{"code_mode_enabled":false}`, enabled: false},
		{contents: `{}`, enabled: true},
	} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.ReloadSettings(); err != nil {
			t.Fatal(err)
		}
		if _, ok := a.services.registry.Get("RunCode"); ok != (tc.enabled && codemode.Available) {
			t.Fatalf("RunCode registered=%v after reload of %s", ok, tc.contents)
		}
	}
}

func TestCodeModeDependentToolsJournalAndRecovery(t *testing.T) {
	a, calls, events := codeAgent(t, `const r=await tools.Read({file_path:"input.txt"}); text({lines:r.data.lines.length,bytes:r.text.length}); await tools.Write({file_path:"result.txt",content:String(r.data.lines.length)});`, nil)
	if err := os.WriteFile(filepath.Join(a.services.cfg.Workspace, "input.txt"), []byte(strings.Repeat(strings.Repeat("x", 90)+"\n", 200)), 0600); err != nil {
		t.Fatal(err)
	}
	finishCode(t, a, events)
	if calls.Load() != 2 {
		t.Fatalf("model calls = %d", calls.Load())
	}
	b, err := os.ReadFile(filepath.Join(a.services.cfg.Workspace, "result.txt"))
	if err != nil || string(b) != "200" {
		t.Fatalf("result %q %v; history %+v", b, err, a.History())
	}
	modelResults := 0
	for _, m := range a.History() {
		if m.Role == messages.RoleTool {
			modelResults++
			if m.ToolCallID != "code-parent" {
				t.Fatalf("child leaked to model: %+v", m)
			}
		}
	}
	if modelResults != 1 {
		t.Fatalf("model results %d", modelResults)
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	starts, finishes, data := 0, 0, 0
	var beforeProjection []session.Record
	for _, record := range records {
		switch e := record.Event.(type) {
		case *session.ToolStarted:
			if e.ResultTarget == "program" {
				starts++
				if e.ParentCallID != "code-parent" || e.ChildIndex < 1 {
					t.Fatal(e)
				}
			}
		case *session.ToolFinished:
			if e.ResultTarget == "program" {
				finishes++
				if e.Data != nil {
					data++
				}
			}
		}
		if record.Event.Type() != session.EventTypeToolResultsProjected {
			beforeProjection = append(beforeProjection, record)
		}
	}
	if starts != 2 || finishes != 2 || data != 1 {
		t.Fatalf("starts %d finishes %d data %d", starts, finishes, data)
	}
	recovered, err := replay.Records(beforeProjection)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range recovered.History {
		if m.Role == messages.RoleTool && m.ToolCallID != "code-parent" {
			t.Fatalf("recovery produced orphan: %+v", m)
		}
	}
}

func TestCodeModeHardDenyAndNoRecursiveControl(t *testing.T) {
	a, _, events := codeAgent(t, `text([typeof tools.RunCode,typeof tools.CreateGoal,typeof tools.ToolSearch]); try {await tools.Write({file_path:"blocked",content:"bad"})}catch(e){text(e.status)}`, func(c *config.Config) { c.AlwaysDeny = []string{"Write"} })
	finishCode(t, a, events)
	if _, err := os.Stat(filepath.Join(a.services.cfg.Workspace, "blocked")); !os.IsNotExist(err) {
		t.Fatal("hard-denied file written")
	}
	var output string
	for _, m := range a.History() {
		if m.Role == messages.RoleTool {
			output = m.Content
		}
	}
	if !strings.Contains(output, `"denied":1`) || !strings.Contains(output, `"undefined","undefined","undefined"`) {
		t.Fatal(output)
	}
	records, _ := a.persistenceHandle().Read(session.Beginning)
	for _, r := range records {
		if e, ok := r.Event.(*session.ToolFinished); ok && e.ResultTarget == "program" {
			if e.Status != "denied" || e.Call == nil {
				t.Fatalf("denial missing identity: %+v", e)
			}
		}
	}
}

func TestCodeModeFrozenDirectory(t *testing.T) {
	a, _, _ := codeAgent(t, `text("unused")`, func(cfg *config.Config) { cfg.AlwaysAllow = []string{"Frozen"} })
	old := &codeFixtureTool{name: "Frozen", text: "old"}
	a.services.registry.Register(old)
	lease := a.services.registry.Acquire()
	defer lease.Close()
	a.services.registry.Register(&codeFixtureTool{name: "Frozen", text: "new"})
	a.services.registry.Register(&codeFixtureTool{name: "Late", text: "late"})
	jc := toolJournalContextForStep(testToolStep(t, a))
	jc.toolLease, jc.allowedTools = lease, []string{"Frozen"}
	out, err := a.runCode(context.Background(), messages.ToolCall{ID: "test"}, jc, `text((await tools.Frozen({})).text);text(typeof tools.Late)`, time.Second*5)
	if err != nil || len(out.Outputs) != 2 || string(out.Outputs[0]) != `"old"` || string(out.Outputs[1]) != `"undefined"` {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestCodeModeBashHandleSurvivesCell(t *testing.T) {
	a, _, _ := codeAgent(t, `text("unused")`, nil)
	lease := a.services.registry.Acquire()
	defer lease.Close()
	jc := toolJournalContextForStep(testToolStep(t, a))
	jc.toolLease, jc.allowedTools = lease, []string{"Bash", "Process"}
	summary, err := a.runCode(context.Background(), messages.ToolCall{ID: "background"}, jc, `await tools.Bash({command:"sleep 0.2; printf ready",background:true});`, 5*time.Second)
	if err != nil {
		t.Fatalf("%+v %v", summary, err)
	}
	if len(summary.Processes) != 1 {
		t.Fatalf("missing retained handle: %+v", summary)
	}
	id := summary.Processes[0]
	result, failed := executeTestTool(t, a, messages.ToolCall{ID: "follow-process", Name: "Process", Arguments: map[string]any{"id": id, "action": "wait"}})
	if failed || !strings.Contains(result, "ready") {
		t.Fatalf("%s failed=%v", result, failed)
	}
	_, failed = executeTestTool(t, a, messages.ToolCall{ID: "stop-process", Name: "Process", Arguments: map[string]any{"id": id, "action": "stop"}})
	if failed {
		t.Fatal("could not release retained process")
	}
}

type codeFixtureTool struct{ name, text string }

func (*codeFixtureTool) ProgramCallable() bool { return true }

func (t *codeFixtureTool) Name() string               { return t.name }
func (t *codeFixtureTool) Description() string        { return "fixture" }
func (t *codeFixtureTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t *codeFixtureTool) Execute(ctx *tools.Context, args map[string]any) (tools.Result, error) {
	ctx = ctx.WithArgs(args)
	text, err := func() (string, error) { return t.text, nil }()
	return tools.Result{Text: text}, err
}
