package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/session"

	"github.com/egoist/mygo/ui"
)

type fakeRuntime struct {
	view   protocol.SessionView
	closes atomic.Int32
	submit func(protocol.Command) (protocol.Receipt, error)
}

func (f *fakeRuntime) Snapshot(context.Context) (protocol.SessionView, error) { return f.view, nil }
func (f *fakeRuntime) Watch(ctx context.Context, _ protocol.Cursor) (protocol.Subscription, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeRuntime) Submit(_ context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	return f.submit(cmd)
}
func (f *fakeRuntime) Sessions() protocol.SessionDirectory { return nil }
func (f *fakeRuntime) Close()                              { f.closes.Add(1) }

func TestHostReusesSessionAndClosesOnce(t *testing.T) {
	path := t.TempDir()
	fake := &fakeRuntime{view: protocol.SessionView{SessionID: "saved"}}
	h := newHost(config.Default())
	var opened atomic.Int32
	h.open = func(cfg config.Config, id string) (runtimeSession, error) {
		opened.Add(1)
		return fake, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := h.session(context.Background(), path, "saved", draftOptions{})
			if err != nil || got != fake {
				t.Errorf("open = %v, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if opened.Load() != 1 {
		t.Fatalf("opened %d runtimes for one log", opened.Load())
	}
	h.close()
	h.close()
	if fake.closes.Load() != 1 {
		t.Fatalf("closed %d times", fake.closes.Load())
	}
	if _, _, err := h.session(context.Background(), path, "saved", draftOptions{}); err == nil {
		t.Fatal("opened after shutdown")
	}
}

func TestCancelledConstructionReleasesRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeRuntime{view: protocol.SessionView{SessionID: "new"}}
	h := newHost(config.Default())
	h.open = func(config.Config, string) (runtimeSession, error) { cancel(); return fake, nil }
	if _, _, err := h.session(ctx, t.TempDir(), "", draftOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if fake.closes.Load() != 1 || len(h.entries) != 0 {
		t.Fatal("cancelled open leaked a runtime")
	}
}

func TestProjectionUsesCumulativeItemsAndAuthoritativeState(t *testing.T) {
	p := projection{view: protocol.SessionView{SessionID: "s"}}
	for _, text := range []string{"你", "你好", "你好，世界"} {
		p.apply(protocol.Update{Event: &protocol.EventView{SessionID: "s", Kind: protocol.EventStream,
			Text: "must not append", Transcript: &protocol.TranscriptItem{ID: "reply", Kind: "assistant", Text: text}}})
	}
	if len(p.view.Transcript) != 1 || p.view.Transcript[0].Text != "你好，世界" {
		t.Fatalf("duplicated cumulative stream: %+v", p.view.Transcript)
	}
	p.apply(protocol.Update{Event: &protocol.EventView{SessionID: "other", Transcript: &protocol.TranscriptItem{ID: "wrong"}}})
	if len(p.view.Transcript) != 1 {
		t.Fatal("mixed another session's output")
	}
	p.upsert(protocol.TranscriptItem{ID: "committed", PreviousID: "reply", Kind: "assistant", Text: "你好，世界", Status: "completed"})
	if len(p.view.Transcript) != 1 || p.view.Transcript[0].ID != "committed" {
		t.Fatal("durable commit duplicated the live reply")
	}
	p.view.Approval = &protocol.ApprovalView{ID: "approval"}
	p.apply(protocol.Update{State: &protocol.SessionState{SessionID: "s", Phase: protocol.PhaseIdle}})
	if p.view.Approval != nil || len(p.view.Transcript) != 0 {
		t.Fatal("state did not clear decision and replaced transcript")
	}
}

func testApp(t *testing.T) (*app, *conversation, chan func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	callbacks := make(chan func(), 32)
	conv := &conversation{projection: projection{view: protocol.SessionView{
		SessionID: "s", Phase: protocol.PhaseIdle,
		Settings: protocol.SettingsSnapshot{Workspace: "/work/ccdp", Model: protocol.ModelBinding{Model: "provider/model"}, Permission: protocol.PermissionPolicy{Mode: "auto"}},
	}}, title: "新会话", rootID: "s", connected: true, expanded: make(map[string]bool)}
	a := &app{host: newHost(config.Default()), ctx: ctx, cancel: cancel, current: "s", workspace: "/work/ccdp", configPath: "~/.ccdp/config.json", sidebar: true,
		conversations: map[string]*conversation{"s": conv}, dispatch: func(fn func()) { callbacks <- fn }}
	var err error
	a.projects, err = loadProjectStore(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a, conv, callbacks
}

func applyCallback(t *testing.T, callbacks <-chan func()) {
	t.Helper()
	select {
	case fn := <-callbacks:
		fn()
	case <-time.After(3 * time.Second):
		t.Fatal("callback timed out")
	}
}

func TestUncertainSendRetriesSameCommandAndPreservesNewDraft(t *testing.T) {
	a, conv, callbacks := testApp(t)
	commands := make(chan protocol.Command, 2)
	attempts := 0
	conv.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		commands <- cmd
		attempts++
		if attempts == 1 {
			return protocol.Receipt{}, errors.New("connection lost after admission")
		}
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}}
	conv.draft = "修复中文输入"
	a.send(conv)
	applyCallback(t, callbacks)
	if conv.pending == nil || conv.draft != "修复中文输入" {
		t.Fatal("uncertain send lost its identity or draft")
	}
	a.send(conv)
	conv.draft = "新的草稿"
	applyCallback(t, callbacks)
	first, second := <-commands, <-commands
	if !reflect.DeepEqual(first, second) {
		t.Fatal("retry changed the admitted command envelope")
	}
	if conv.pending != nil || conv.draft != "新的草稿" {
		t.Fatal("receipt overwrote a newer draft")
	}
}

func TestDraftNavigationPreservesWorkspaceAndText(t *testing.T) {
	a, _, _ := testApp(t)
	a.newConversation()
	if entries := a.allSessionEntries(); len(entries) != 0 {
		t.Fatalf("opening a blank composer created a sidebar row: %+v", entries)
	}
	blank := a.current
	a.active().draft = " \n\t"
	a.send(a.active())
	if a.opening || len(a.allSessionEntries()) != 0 {
		t.Fatal("whitespace appeared in history or opened a runtime")
	}
	a.newConversation()
	if _, ok := a.conversations[blank]; ok {
		t.Fatal("navigating away retained an empty draft")
	}
	first := a.current
	a.active().draft = "保留这个草稿"
	a.workspace = "/work/another"
	a.newConversation()
	a.active().draft = "另一个项目的草稿"
	if entries := a.allSessionEntries(); len(entries) != 2 || !entries[0].Draft || !entries[1].Draft {
		t.Fatalf("expected two unsent drafts: %+v", entries)
	}
	a.openSession("", first)
	if a.workspace != "/work/ccdp" || a.active().draft != "保留这个草稿" {
		t.Fatal("navigation lost the draft or its project")
	}
	a.openProjectPage(a.workspace, false)
	if a.projectPage.sessionCount != 0 {
		t.Fatal("a project counted a draft as a chat")
	}
}

func TestLazyOpenSendsOnlyTheSubmittedDraft(t *testing.T) {
	a, _, callbacks := testApp(t)
	cfg := config.Default()
	cfg.Model = "example/first"
	cfg.ReasoningEffort = "high"
	cfg.Providers = map[string]config.ProviderConfig{"example": {ModelConfigs: map[string]config.ModelConfig{
		"first": {SupportedReasoningEfforts: []string{"high"}}, "second": {},
	}}}
	a.host = newHost(cfg)
	a.workspace = ""
	a.newConversation()
	conv := a.active()
	tt := ui.NewTester(a.view, 780, 580)
	picker, ok := tt.AccessByRole(ui.RolePopUpButton, "模型")
	if !ok || picker.Value != cfg.Model {
		t.Fatalf("initial model picker = %+v", picker)
	}
	mode, ok := tt.AccessByRole(ui.RolePopUpButton, "权限模式")
	label, showsEffort := tt.Find("高")
	if !ok || mode.Value != "auto" || mode.Bounds.X+mode.Bounds.W >= picker.Bounds.X || !showsEffort ||
		label.X < picker.Bounds.X || label.X+label.W > picker.Bounds.X+picker.Bounds.W {
		t.Fatalf("incorrect composer controls: mode=%+v model=%+v effort=%+v", mode, picker, label)
	}
	for _, label := range []string{"权限模式", "manual", "模型", "切换模型", "example/second"} {
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	if tt.HasText("高") || conv.draftMode != "manual" {
		t.Fatal("an unknown model kept the last model's effort or lost the permission selection")
	}
	if len(a.allSessionEntries()) != 0 || len(a.host.entries) != 0 || a.opening {
		t.Fatal("choosing a model before typing created a chat")
	}
	conv.draft = "提交的内容"
	a.openProjectForChat(t.TempDir(), conv)
	if a.active() != conv || conv.view.Settings.Workspace != a.workspace {
		t.Fatal("choosing a project replaced the current draft")
	}
	commands := make(chan protocol.Command, 1)
	runtime := &fakeRuntime{view: protocol.SessionView{SessionID: "created", Settings: protocol.SettingsSnapshot{
		Workspace: a.workspace, Permission: protocol.PermissionPolicy{Mode: "auto", AlwaysDeny: []string{"Bash"}},
	}}}
	runtime.submit = func(cmd protocol.Command) (protocol.Receipt, error) {
		if cmd.Type == protocol.CommandSetPermissionPolicy {
			runtime.view.Settings.Permission = cmd.PermissionPolicy.Policy
		} else {
			if policy := runtime.view.Settings.Permission; policy.Mode != "manual" || !reflect.DeepEqual(policy.AlwaysDeny, []string{"Bash"}) {
				t.Errorf("first input used policy %+v", policy)
			}
			commands <- cmd
		}
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}
	a.host.open = func(cfg config.Config, _ string) (runtimeSession, error) {
		if cfg.Model != "example/second" {
			t.Errorf("first send used model %q", cfg.Model)
		}
		return runtime, nil
	}
	t.Cleanup(a.host.close)
	a.send(conv)
	conv.draft = "打开运行时期间的新草稿"
	applyCallback(t, callbacks)
	if entries := a.allSessionEntries(); len(entries) != 1 || !entries[0].Draft {
		t.Fatalf("runtime creation counted a send before admission: %+v", entries)
	}
	applyCallback(t, callbacks)
	if command := <-commands; command.Input.Text != "提交的内容" {
		t.Fatalf("sent newer draft: %q", command.Input.Text)
	}
	if a.active().draft != "打开运行时期间的新草稿" {
		t.Fatal("opening runtime discarded newer draft")
	}
	if entries := a.allSessionEntries(); len(entries) != 1 || entries[0].Draft || entries[0].Title != "提交的内容" {
		t.Fatalf("first accepted input did not become one named chat: %+v", entries)
	}
	a.openProjectPage(a.workspace, false)
	if a.projectPage.sessionCount != 1 {
		t.Fatal("project failed to count the first accepted message")
	}
}

func TestSavedSessionsSkipUnusedRuntime(t *testing.T) {
	cfg := config.Default()
	cfg.SessionDir = t.TempDir()
	now := time.Now()
	for _, id := range []string{"blank", "pending", "delivered"} {
		store, err := session.OpenJSONLStore(cfg.SessionDir, id)
		if err != nil {
			t.Fatal(err)
		}
		events := []session.Event{session.SessionCreated{SessionID: id, CreatedAt: now, FormatVersion: session.SchemaVersion}}
		if id != "blank" {
			events = append(events, session.InputQueued{InputID: "first", Text: "第一条消息", Strategy: "steer"})
		}
		if id == "delivered" {
			events = append(events, session.InputDelivered{InputID: "first", TurnID: "turn-1"})
		}
		_, err = store.Commit(session.Beginning, session.Batch{TransactionID: "setup", Events: events})
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	broken := filepath.Join(cfg.SessionDir, "broken")
	if err := os.Mkdir(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "events.v1.jsonl"), []byte("{not-json}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := newHost(cfg).saved()
	if err != nil || len(entries) != 2 {
		t.Fatalf("history after restart: %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if entry.ID == "blank" || entry.Draft || entry.Title != "第一条消息" {
			t.Fatalf("unexpected saved chat: %+v", entry)
		}
	}
}

func TestSidebarKeepsNewestChatFirstAcrossNavigation(t *testing.T) {
	a, root, callbacks := testApp(t)
	dir := t.TempDir()
	now := time.Now()
	a.saved = []savedSession{
		{ID: "older", Title: "旧对话", Workspace: dir, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Hour)},
		{ID: "s", Title: "新对话", Workspace: dir, CreatedAt: now, UpdatedAt: now},
	}
	root.client, root.submitted = &fakeRuntime{}, true
	root.view.Settings.Workspace = dir
	root.title = "新对话"
	a.host.open = func(config.Config, string) (runtimeSession, error) {
		return &fakeRuntime{view: protocol.SessionView{SessionID: "older", Settings: root.view.Settings}}, nil
	}
	t.Cleanup(a.host.close)
	assertOrder := func() {
		t.Helper()
		rows := a.allSessionEntries()
		if len(rows) != 2 || rows[0].ID != "s" || rows[1].ID != "older" {
			t.Fatalf("wrong chat order: %+v", rows)
		}
	}
	assertOrder()
	a.openSession(dir, "older")
	applyCallback(t, callbacks)
	assertOrder()
	a.conversations = map[string]*conversation{} // Reproject from disk.
	assertOrder()
}

func TestSidebarOnlyMarksBackgroundExecution(t *testing.T) {
	a, root, _ := testApp(t)
	root.client, root.submitted, root.view.Busy = &fakeRuntime{}, true, true
	root.title = "进行中的对话"
	tt := ui.NewTester(a.view, 1180, 800)
	check := func(want bool) {
		t.Helper()
		tt.Frame()
		if row, ok := tt.AccessByRole(ui.RoleButton, root.title); !ok || row.Busy != want {
			t.Fatalf("sidebar running indicator: row found=%v, busy=%v, want %v", ok, row.Busy, want)
		}
	}
	check(false)
	a.newConversation()
	check(true)
	root.view.Busy = false
	root.children = []protocol.ChildSession{{Run: protocol.RunView{Status: "running"}}}
	check(true)
	root.children[0].Run.Status = "succeeded"
	root.view.OutstandingWork = 1 // A pending delivery can outlive execution.
	check(false)
	root.view.Busy, root.connected = true, false
	check(false)
}

func TestDesktopComposerAndApproval(t *testing.T) {
	a, conv, callbacks := testApp(t)
	commands := make(chan protocol.Command, 4)
	conv.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		commands <- cmd
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}}
	tt := ui.NewTester(a.view, 1180, 800)
	if err := tt.Click("消息"); err != nil {
		t.Fatal(err)
	}
	tt.Type("检查项目")
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Type("并运行测试")
	if conv.draft != "检查项目\n并运行测试" {
		t.Fatalf("draft = %q", conv.draft)
	}
	tt.Key(0, ui.KeyEnter)
	applyCallback(t, callbacks)
	if cmd := <-commands; cmd.Input.Text != "检查项目\n并运行测试" {
		t.Fatalf("input = %+v", cmd.Input)
	}
	conv.view.Approval = &protocol.ApprovalView{ID: "gate", Tool: "Bash", Reason: "运行项目测试", Args: jsonBytes(`{"command":"go test ./..."}`), Capabilities: []protocol.CapabilityRequest{{Kind: "directory", Access: "write", Path: "/private/tmp"}}}
	tt.Frame()
	if !tt.HasText("主会话 · Bash 请求批准") || !tt.HasText("写入目录 /private/tmp") {
		t.Fatal("approval did not identify the requesting session and resource")
	}
	if err := tt.Click("允许一次"); err != nil {
		t.Fatal(err)
	}
	applyCallback(t, callbacks)
	cmd := <-commands
	if cmd.Approval == nil || !cmd.Approval.Approve || cmd.Approval.CapabilityScope != protocol.CapabilityScopeOnce {
		t.Fatalf("approval = %+v", cmd)
	}
}

func jsonBytes(text string) []byte { return []byte(text) }

func TestComposerPermissionPicker(t *testing.T) {
	a, conv, callbacks := testApp(t)
	commands := make(chan protocol.Command, 1)
	conv.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		commands <- cmd
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}}
	conv.view.Settings.Revision = 7
	conv.view.Revision.LogSeq = 21
	conv.view.Settings.Permission.AlwaysDeny = []string{"Bash"}
	tt := ui.NewTester(a.view, 780, 580)
	for _, next := range []string{"manual", "auto"} {
		picker, ok := tt.AccessByRole(ui.RolePopUpButton, "权限模式")
		if !ok || picker.Value != conv.view.Settings.Permission.Mode || picker.Bounds.W < 88 || picker.Bounds.W > 110 {
			t.Fatalf("expected an icon and mode with a comfortable hit target, got %+v", picker)
		}
		if err := tt.Click("权限模式"); err != nil {
			t.Fatal(err)
		}
		if err := tt.Click(next); err != nil {
			t.Fatal(err)
		}
		applyCallback(t, callbacks)
		cmd := <-commands
		if cmd.Type != protocol.CommandSetPermissionPolicy || cmd.ExpectedRevision != 21 || cmd.PermissionPolicy == nil {
			t.Fatalf("unexpected permission command: %+v", cmd)
		}
		expected := conv.view.Settings.Permission
		expected.Mode = next
		if !reflect.DeepEqual(cmd.PermissionPolicy.Policy, expected) {
			t.Fatalf("updated permission policy: %+v", cmd.PermissionPolicy.Policy)
		}
		conv.view.Settings.Permission = cmd.PermissionPolicy.Policy
		tt.Frame()
	}
}

func TestWindowToolbarClearsNativeControls(t *testing.T) {
	a, _, _ := testApp(t)
	const width = 780
	tt := ui.NewTester(a.view, width, 580)
	native := ui.TitleBar{Left: 76, Height: 44, Bottom: 29}
	tt.SetTitleBar(native)
	bar, ok := tt.AccessByRole(ui.RoleToolbar, "窗口工具栏")
	if !ok || bar.Bounds != (ui.Rect{W: width, H: native.Height}) {
		t.Fatalf("expected a full-width title bar at the top, got %+v", bar.Bounds)
	}
	home, ok := tt.Find("对话")
	if !ok || home.Y < bar.Bounds.H || home.Y >= bar.Bounds.H+16 {
		t.Fatalf("navigation should start immediately below the title bar: %+v", home)
	}
	for _, label := range []string{"收起侧栏", "显示侧栏"} {
		button, ok := tt.Find(label)
		if !ok || button.X <= native.Left || button.Y < 0 || button.Y+button.H > bar.Bounds.H {
			t.Fatalf("%s overlaps native controls or escapes the toolbar: %+v", label, button)
		}
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	captureSidebar(t, tt, "window-toolbar-layout.png")
	tt.SetTitleBar(ui.TitleBar{}) // macOS hides the traffic lights in full screen.
	if toggle, ok := tt.Find("收起侧栏"); !ok || toggle.X >= native.Left {
		t.Fatalf("full-screen title bar still reserves space for traffic lights: %+v", toggle)
	}
}

func TestDesktopLayout(t *testing.T) {
	a, conv, _ := testApp(t)
	conv.client = &fakeRuntime{}
	tt := ui.NewTester(a.view, 1180, 800)
	for _, name := range []string{"消息", "发送消息", "打开项目", "新会话"} {
		if _, ok := tt.Find(name); !ok {
			t.Errorf("missing %s", name)
		}
	}
	if box, ok := tt.Find("设置"); !ok || box.Y < 700 {
		t.Fatalf("sidebar does not fill the window: %+v", box)
	}
	if box, ok := tt.Find("发送消息"); !ok || box.Y < 650 {
		t.Fatalf("composer is not bottom anchored: %+v", box)
	}
	if dir := os.Getenv("CCDP_DESKTOP_SCREENSHOTS"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		write := func(name string) {
			f, err := os.Create(filepath.Join(dir, name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := png.Encode(f, tt.Image()); err != nil {
				t.Fatal(err)
			}
		}
		a.newConversation()
		tt.Frame()
		write("welcome")
		a.selectConversation("s")
		conv.title = "梳理项目结构"
		conv.view.HistoryLength = 3
		conv.view.Transcript = []protocol.TranscriptItem{
			{ID: "u", Kind: "user", Text: "介绍这个项目的结构，以及桌面端如何接入现有运行时。", Status: "completed"},
			{ID: "t", Kind: "tool", Tool: "Read", Text: "module ccdp\n\ngo 1.27.3", Status: "success"},
			{ID: "a", Kind: "assistant", Text: "ccdp 的核心是一个 **Go 编写的本地开发助手**。桌面端可以复用已有的会话协议。\n\n### 代码结构\n\n- `internal/agent` — 模型调用、工具执行与任务调度\n- `internal/protocol` — 会话状态、命令与流式事件\n- `internal/desktop` — 原生界面和会话管理\n\n```go\nclient.Submit(ctx, protocol.NewSubmitInput(\n    commandID, sessionID, inputID, text, protocol.InputSteer,\n))\n```\n\n界面只处理展示和输入，审批与执行继续由运行时统一管理。", Status: "completed"},
		}
		tt.Frame()
		write("conversation")
		tt.SetDark(true)
		write("conversation-dark")
	}
	tt.SetSize(780, 580)
	if box, ok := tt.Find("发送消息"); !ok || box.W < 20 || box.H < 20 || box.Y+box.H > 580 {
		t.Fatalf("composer unavailable at minimum size: %+v", box)
	}
}

func TestDraftComposerFitsItsText(t *testing.T) {
	a, _, _ := testApp(t)
	a.newConversation()
	conv := a.active()
	tt := ui.NewTester(a.view, 1180, 800)
	for _, removed := range []string{"使用已配置的模型", "Shift + Enter 换行", "一起把想法变成代码", "ccdp"} {
		if _, ok := tt.Find(removed); ok {
			t.Errorf("unexpected composer copy %q", removed)
		}
	}
	card, ok := tt.Find("消息编辑区")
	if !ok || card.W != 740 || card.H < 94 || card.H > 104 {
		t.Fatalf("expected a two-line composer, got %+v", card)
	}
	input, ok := tt.Access("消息")
	if !ok || input.Placeholder != "和 ccdp 一起工作" || input.Value != "" {
		t.Fatalf("unexpected empty message field: %+v", input)
	}
	tt.Type("直接输入")
	if conv.draft != "直接输入" {
		t.Fatal("new draft did not have keyboard focus")
	}
	tt.SetSize(780, 580)
	conv.draft = "第一行\n第二行\n第三行"
	tt.Frame()
	three, _ := tt.Find("消息")
	conv.draft = strings.Repeat("多行文本\n", 30)
	tt.Frame()
	capped, _ := tt.Find("消息")
	if three.H <= input.Bounds.H || capped.H <= three.H || capped.H > 135 {
		t.Fatalf("unexpected composer growth: empty=%v three=%v long=%v", input.Bounds.H, three.H, capped.H)
	}
	conv.draft = ""
	tt.Frame()
	if empty, _ := tt.Find("消息"); empty.H != input.Bounds.H {
		t.Fatalf("cleared input did not shrink: %+v", empty)
	}
}

func TestDesktopRuntimeRoundTrip(t *testing.T) {
	if os.Getenv("CCDP_DESKTOP_INTEGRATION") != "1" {
		t.Skip("requires the macOS host Seatbelt backend; set CCDP_DESKTOP_INTEGRATION=1")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "fast" || body.Effort != "high" {
			t.Errorf("expected the draft selection on the wire, got %+v, %v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"桌面端已连接运行时。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.Workspace, cfg.SessionDir = t.TempDir(), t.TempDir()
	cfg.RuntimeRoot = filepath.Join(t.TempDir(), "runtime")
	cfg.Model = "desktop-test/model"
	cfg.Providers = map[string]config.ProviderConfig{"desktop-test": {
		BaseURL: server.URL, APIKey: "test", WireAPI: "chat",
		ModelConfigs: map[string]config.ModelConfig{
			"model": {ContextWindow: 128000, MaxOutputTokens: 4096},
			"fast":  {ContextWindow: 128000, MaxOutputTokens: 4096, SupportedReasoningEfforts: []string{"medium", "high", "xhigh"}},
		},
	}}
	cfg.SystemPrompt = "Reply to the user."
	h := newHost(cfg)
	defer h.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runtime, initial, err := h.session(ctx, cfg.Workspace, "", draftOptions{model: "desktop-test/fast", mode: "manual", effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Settings.Permission.Mode != "manual" || !initial.Settings.ReasoningEffortSupported || initial.Settings.ReasoningEffort != "high" {
		t.Fatal("first turn did not receive the draft's permission and model settings")
	}
	if entries, err := h.saved(); err != nil || len(entries) != 0 {
		t.Fatalf("new runtime appeared in history before a send: %+v, %v", entries, err)
	}
	sub, err := runtime.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	cmd := protocol.NewSubmitInput("desktop-test", initial.SessionID, "input-test", "你好", protocol.InputSteer)
	receipt, err := runtime.Submit(ctx, cmd)
	if err != nil || receipt.Rejected() {
		t.Fatalf("submit: %+v, %v", receipt, err)
	}
	p := projection{}
	for p.view.LastTurn == nil || p.view.LastTurn.Status == protocol.TurnRunning {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				t.Fatal("watch closed before completion")
			}
			p.apply(update)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if p.view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("turn failed: %+v", p.view.LastTurn)
	}
	if !slicesContainReply(p.view.Transcript, "桌面端已连接运行时。") {
		t.Fatalf("missing streamed reply: %+v", p.view.Transcript)
	}
	after, err := runtime.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effort := "xhigh"
	change := protocol.Command{ID: "live-effort", SessionID: initial.SessionID, ExpectedRevision: after.Revision.LogSeq, Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}}
	if receipt, err := runtime.Submit(ctx, change); err != nil || !receipt.Applied() {
		t.Fatalf("effort switch: %+v, %v", receipt, err)
	}
	_ = sub.Close()
	h.close()
	reopened := newHost(cfg)
	defer reopened.close()
	if entries, err := reopened.saved(); err != nil || len(entries) != 1 || entries[0].Title != "你好" {
		t.Fatalf("saved history did not contain exactly the submitted chat: %+v, %v", entries, err)
	}
	_, restored, err := reopened.session(ctx, cfg.Workspace, initial.SessionID.String(), draftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Settings.Model.Model != "desktop-test/fast" || restored.Settings.ReasoningEffort != "xhigh" {
		t.Fatalf("restored model: %q", restored.Settings.Model.Model)
	}
	if !slicesContainReply(restored.Transcript, "桌面端已连接运行时。") {
		t.Fatalf("missing restored reply: %+v", restored.Transcript)
	}
	reopened.close()
	delete(cfg.Providers["desktop-test"].ModelConfigs, "fast")
	reconfigured := newHost(cfg)
	defer reconfigured.close()
	_, migrated, err := reconfigured.session(ctx, cfg.Workspace, initial.SessionID.String(), draftOptions{})
	if err != nil || migrated.Settings.Model.Model != cfg.Model || !slicesContainReply(migrated.Transcript, "桌面端已连接运行时。") {
		t.Fatalf("could not reopen chat after removing its model: %q, %v", migrated.Settings.Model.Model, err)
	}
}

func TestSavePreferencesDuringProviderRequest(t *testing.T) {
	if os.Getenv("CCDP_DESKTOP_INTEGRATION") != "1" {
		t.Skip("requires local HTTP and the macOS runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	deferRelease := sync.OnceFunc(func() { close(release) })
	reply := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer old-fixture" {
			t.Error("first request did not use the original credentials")
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		reply(w)
	}))
	defer oldServer.Close()
	requests := make(chan string, 4)
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Authorization") != "Bearer new-fixture" {
			t.Error("new endpoint did not receive the updated credentials and request")
		}
		requests <- body.Model
		reply(w)
	}))
	defer newServer.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	initial := fmt.Sprintf(`{"model":"example/coder","providers":{"example":{"base_url":%q,"wire_api":"chat","api_key":"old-fixture","models":{"coder":{"context_window":32768,"max_output_tokens":1024}}}}}`, oldServer.URL)
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace, cfg.SessionDir, cfg.RuntimeRoot = t.TempDir(), filepath.Join(dir, "sessions"), filepath.Join(dir, "runtime")
	cfg.SystemPrompt = "Reply briefly."
	a, _, callbacks := testApp(t)
	a.host, a.configPath = newHost(cfg), path
	defer a.host.close()
	defer deferRelease() // Release the fixture before closing a busy runtime on test failure.
	client, view, err := a.host.session(ctx, cfg.Workspace, "", draftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	conv := &conversation{client: client, rootID: view.SessionID.String(), projection: projection{view: view}}
	a.conversations = map[string]*conversation{conv.rootID: conv}
	input := func(id string) {
		t.Helper()
		cmd := protocol.NewSubmitInput(protocol.CommandID(id), view.SessionID, protocol.InputID(id), id, protocol.InputSteer)
		if receipt, err := client.Submit(ctx, cmd); err != nil || receipt.Rejected() {
			t.Fatalf("input %s: %+v, %v", id, receipt.Error, err)
		}
	}
	input("first")
	select {
	case <-started:
	case <-ctx.Done():
		status, _ := client.Snapshot(context.Background())
		t.Fatalf("request did not reach the fixture: %+v", status.LastTurn)
	}
	form, err := loadConfigForm(path)
	if err != nil {
		t.Fatal(err)
	}
	p := form.Providers[0]
	p.BaseURL, p.APIKey = newServer.URL, "new-fixture"
	p.Models[0].ContextWindow = "65536"
	p.Models = append(p.Models, &modelForm{ID: "other"})
	form.selectDefaultModel("example/other")
	form.selectFallbackModel("example/coder")
	form.PermissionMode = "manual"
	save := func() protocol.SessionView {
		t.Helper()
		if err := form.save(path); err != nil {
			t.Fatal(err)
		}
		a.applySavedConfig(form.resolved)
		applyCallback(t, callbacks)
		if conv.configError != "" || conv.configApplying {
			t.Fatalf("refresh failed: %s", conv.configError)
		}
		v, err := client.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	updated := save()
	if !updated.Busy || updated.Settings.Model.Model != "example/coder" || updated.Settings.ContextWindow != 65536 ||
		updated.Settings.Permission.Mode != "manual" || a.host.settings.Load().Model != "example/other" {
		t.Fatalf("wrong live settings: busy=%t model=%s window=%d mode=%s", updated.Busy, updated.Settings.Model.Model, updated.Settings.ContextWindow, updated.Settings.Permission.Mode)
	}
	deferRelease()
	input("second")
	select {
	case model := <-requests:
		if model != "coder" {
			t.Fatalf("unexpected model after refresh: %s", model)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Removing the active model must bind the surviving default in this chat.
	p.Models = p.Models[1:]
	afterRemoval := save()
	if afterRemoval.Settings.Model.Model != "example/other" || len(afterRemoval.Catalog.Providers) != 1 || len(afterRemoval.Catalog.Providers[0].Models) != 1 {
		t.Fatal("removed model still active or present in the live picker")
	}
}

func slicesContainReply(items []protocol.TranscriptItem, text string) bool {
	for _, item := range items {
		if item.Kind == "assistant" && item.Text == text {
			return true
		}
	}
	return false
}

func TestTurnErrorAppearsOnce(t *testing.T) {
	const msg = "incomplete tool call"
	p := projection{}
	event := protocol.Update{Event: &protocol.EventView{Kind: protocol.EventError, TurnID: "2", Text: msg}}
	p.apply(event)
	state := protocol.SessionState{Transcript: []protocol.TranscriptItem{{ID: "error:turn-1", TurnID: "turn-1", Kind: "error", Text: msg}}}
	p.apply(protocol.Update{State: &state})
	if p.notice != msg {
		t.Fatal("old error hid a new notice")
	}
	state.Transcript[0].ID, state.Transcript[0].TurnID = "error:turn-2", "turn-2"
	p.apply(protocol.Update{State: &state})
	p.apply(event)
	if p.notice != "" || len(p.view.Transcript) != 1 {
		t.Fatal("turn error repeated above transcript")
	}
}
