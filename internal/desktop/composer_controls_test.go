package desktop

import (
	"fmt"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

func TestEffortSliderAndUsageIndicators(t *testing.T) {
	a, _, callbacks := testApp(t)
	cfg := config.Default()
	cfg.Model = "olm/claude-opus-5-5"
	cfg.Providers = map[string]config.ProviderConfig{"olm": {ModelConfigs: map[string]config.ModelConfig{"claude-opus-5-5": {ContextWindow: 128000}, "custom": {SupportedReasoningEfforts: []string{"low"}}}}}
	a.host = newHost(cfg)
	a.workspace = t.TempDir()
	a.newConversation()
	conv := a.active()
	tt := ui.NewTester(a.view, 1180, 800)
	tt.SetScale(2)
	click := func(name string) {
		t.Helper()
		if err := tt.Click(name); err != nil {
			t.Fatal(err)
		}
	}
	click("模型")
	slider, ok := tt.AccessByRole(ui.RoleSlider, "推理强度")
	if !ok || slider.Value != "中" {
		t.Fatalf("slider: %+v", slider)
	}
	captureSidebar(t, tt, "effort-popup.png")
	r := slider.Bounds
	x, y := r.X+r.W-12, r.Y+r.H/2
	tt.ClickAt(x, y)
	if conv.draftEffort != "max" || conv.client != nil {
		t.Fatalf("click did not commit draft effort: %q", conv.draftEffort)
	}
	checkOpen := func(want string) {
		t.Helper()
		slider, ok := tt.AccessByRole(ui.RoleSlider, "推理强度")
		if !ok || slider.Value != want {
			t.Fatalf("expected effort panel to remain open at %q, got %+v", want, slider)
		}
	}
	checkOpen("最高")
	tt.ClickAt(x, y)
	checkOpen("最高")
	tt.Key(0, ui.KeyHome)
	tt.Key(0, ui.KeyEnter)
	checkOpen("低")
	if conv.draftEffort != "low" {
		t.Fatal("Enter did not commit the keyboard selection")
	}
	tt.Key(0, ui.KeyRight)
	tt.Key(0, ui.KeyEscape)
	if _, open := tt.AccessByRole(ui.RoleSlider, "推理强度"); open || conv.draftEffort != "low" {
		t.Fatal("Escape should dismiss the panel and discard the keyboard preview")
	}
	conv.view.Busy = true
	conv.view.RunID = "active-run"
	conv.view.Revision = protocol.Revision{LogSeq: 11, SettingsRev: 1}
	conv.view.Catalog.Providers = []protocol.ProviderSnapshot{{ID: "olm", Models: []string{"custom"}}}
	sent := make(chan protocol.Command, 2)
	conv.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		sent <- cmd
		if cmd.Type == protocol.CommandSetModel {
			return protocol.Receipt{Status: protocol.ReceiptApplied, Revision: protocol.Revision{SettingsRev: 3}}, nil
		}
		if *cmd.Generation.ReasoningEffort == "medium" {
			return protocol.Receipt{Status: protocol.ReceiptRejected, Error: &protocol.CommandError{Message: "设置未生效"}}, nil
		}
		return protocol.Receipt{Status: protocol.ReceiptApplied, Revision: protocol.Revision{LogSeq: 12, SettingsRev: 2}}, nil
	}}
	tt.Frame()
	click("模型")
	tt.Press(x, y)
	tt.Move(r.X+12, y)
	tt.Move(r.X+12+(r.W-24)*.75, y)
	if conv.sending {
		t.Fatal("drag sent a settings command before release")
	}
	tt.Release(r.X+12+(r.W-24)*.75, y)
	applyCallback(t, callbacks)
	cmd := <-sent
	if cmd.Type != protocol.CommandSetGeneration || cmd.ExpectedRevision != 0 || cmd.ExpectedRunID != "active-run" || *cmd.Generation.ReasoningEffort != "xhigh" {
		t.Fatalf("live effort: %+v", cmd)
	}
	conv.view.Revision.LogSeq++ // A transcript update can arrive before the settings update.
	tt.Frame()
	checkOpen("极高")
	if s, _ := tt.Access("推理强度"); !s.Disabled {
		t.Fatal("accepted command still awaiting its settings update")
	}
	conv.view.Revision.SettingsRev = 2
	conv.view.Settings.ReasoningEffort = "xhigh"
	tt.Frame()
	tt.ClickAt(r.X+12+(r.W-24)*.25, y)
	applyCallback(t, callbacks)
	if cmd := <-sent; cmd.ExpectedRevision != 0 || *cmd.Generation.ReasoningEffort != "medium" {
		t.Fatal(cmd)
	}
	tt.Frame()
	checkOpen("极高") // The rejected choice must not look applied.
	click("消息")
	if _, open := tt.AccessByRole(ui.RoleSlider, "推理强度"); open {
		t.Fatal("outside click left the panel open")
	}
	click("模型")
	click("切换模型")
	click("olm/custom")
	applyCallback(t, callbacks)
	if cmd := <-sent; cmd.Type != protocol.CommandSetModel || cmd.ExpectedRevision != 0 || cmd.ExpectedRunID != "active-run" || cmd.Model.Model != "olm/custom" || cmd.Model.ReasoningEffort != "low" {
		t.Fatalf("model switch during execution: %+v", cmd)
	}
	tt.Frame()
	if picker, _ := tt.Access("模型"); !picker.Disabled {
		t.Fatal("model picker re-enabled before receiving the selected model")
	}
	conv.view.Settings.Model.Model, conv.view.Settings.ReasoningEffort = "olm/custom", "low"
	conv.view.Settings.ReasoningEfforts = []string{"low"}
	conv.view.Revision.SettingsRev = 3
	tt.Frame()
	if picker, ok := tt.Access("模型"); !ok || picker.Disabled || picker.Value != "olm/custom" {
		t.Fatalf("model picker did not follow the active session: %+v", picker)
	}

	conv.view.ContextUsedTokens = 850
	conv.view.Usage.Cache = protocol.CacheStats{InputTokens: 12800, ReportedInputTokens: 12800, CachedTokens: 8192, Tracking: true}
	tt.Frame()
	for name, want := range map[string]string{"Context": "Context · 850 / 128k tokens", "Cache read": "Session cache read · 8.2k / 12.8k input tokens"} {
		info, found := tt.Access(name)
		if !found || info.Description != want || tt.HasText(want) {
			t.Fatalf("unexpected %s indicator %+v", name, info)
		}
		tt.Move(info.Bounds.X+info.Bounds.W/2, info.Bounds.Y+info.Bounds.H/2)
		tt.Advance(2 * time.Second)
		if !tt.HasText(want) {
			t.Fatalf("hover did not show %q", want)
		}
		tt.Move(3, 3)
		tt.Advance(time.Second)
	}
	conv.view.Usage.Cache.CachedTokens = 0
	tt.Frame()
	if info, _ := tt.Access("Cache read"); info.Description != "Session cache read · 0 / 12.8k input tokens" {
		t.Fatal(info)
	}
	conv.view.Usage.Cache.ReportedInputTokens = 3200
	tt.Frame()
	if info, _ := tt.Access("Cache read"); info.Description != "Session cache read · unavailable" {
		t.Fatal(info)
	}
	click("权限模式")
	panel, ok := tt.Find("审批模式")
	if !ok || panel.W < 250 || !tt.HasText("在已授权范围内自动执行") {
		t.Fatalf("small mode panel: %+v", panel)
	}
	captureSidebar(t, tt, "mode-popup.png")
}

func TestLongTurnDisclosure(t *testing.T) {
	a, conv, _ := testApp(t)
	conv.client = &fakeRuntime{}
	items := []protocol.TranscriptItem{{ID: "user", Kind: "user", Text: "检查工作区"}}
	for i := range 80 {
		items = append(items, protocol.TranscriptItem{ID: fmt.Sprintf("note%d", i), TurnID: "t", Kind: "assistant", Text: "阶段进展"},
			protocol.TranscriptItem{ID: fmt.Sprintf("tool%d", i), TurnID: "t", Kind: "tool", Tool: "Read", Text: "文件内容", Status: "success"})
	}
	items = append(items, protocol.TranscriptItem{ID: "final", TurnID: "t", Kind: "assistant", Text: "检查完成。"},
		protocol.TranscriptItem{ID: "summary", TurnID: "t", Kind: "turn_summary", Status: "succeeded", DurationMs: 78000})
	conv.view.Transcript = items
	conv.list.FollowEnd = true
	tt := ui.NewTester(a.view, 1180, 800)
	if tt.HasText("Read") || !tt.HasText("检查完成。") || !tt.HasText("检查工作区") {
		t.Fatal("long turn should fit the viewport")
	}
	if err := tt.Click("用时 1 分 18 秒"); err != nil {
		t.Fatal(err)
	}
	rows := transcriptRows(items, conv.expanded)
	if len(rows) != 163 || !rows[2].detail || rows[2].item.ID != "note0" || rows[len(rows)-1].item.ID != "final" {
		t.Fatalf("expanded %d rows", len(rows))
	}
	if err := tt.Click("用时 1 分 18 秒"); err != nil {
		t.Fatal(err)
	}
	// A bounded live snapshot carries the question alongside its retained tail.
	prompt := items[0]
	prompt.TurnID = "t"
	conv.view.TranscriptPrompt = &prompt
	conv.view.TranscriptMore = true
	conv.view.Transcript = items[40:]
	tt.Frame()
	if !tt.HasText("检查工作区") || !tt.HasText("检查完成。") {
		t.Fatal("rolled window lost its question or answer")
	}
	captureSidebar(t, tt, "long-turn.png")
	// Errors and new inputs stay visible, with work in separate disclosures.
	items[80].Kind = "error"
	items[82] = protocol.TranscriptItem{ID: "steer", Kind: "user", Text: "更正范围"}
	rows = transcriptRows(items, nil)
	visible := map[string]bool{}
	for _, row := range rows {
		if row.item != nil {
			visible[row.item.ID] = true
		}
	}
	if !visible[items[80].ID] || !visible["steer"] || !visible["final"] {
		t.Fatal(visible)
	}
}
