package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"github.com/egoist/mygo/ui"
)

func TestHistorySeparatesDelegatedSessionsFromUserForks(t *testing.T) {
	cfg := config.Default()
	cfg.SessionDir = t.TempDir()
	now := time.Now()
	for _, tc := range []struct{ id, source, parent string }{
		{"root", "agent", ""}, {"fork", "agent", ""}, {"task", "task", "root"},
		{"review", "review", "root"}, {"guardian", "guardian", "root"}, {"legacy", "agent", "root"},
	} {
		store, err := session.OpenJSONLStore(cfg.SessionDir, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		events := []session.Event{session.SessionCreated{SessionID: tc.id, Source: tc.source, ParentID: tc.parent, CreatedAt: now, FormatVersion: session.SchemaVersion}}
		if tc.id == "fork" {
			events = append(events, session.SessionImported{SessionID: tc.id, Source: "fork", ParentID: "root", ImportedAt: now, FormatVersion: session.SchemaVersion})
		}
		// Identical titles cannot distinguish a user's chats from a worker.
		events = append(events, session.InputQueued{InputID: "first", Text: "检查项目", Strategy: "followup"})
		_, err = store.Commit(session.Beginning, session.Batch{TransactionID: "setup", Events: events})
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	h := newHost(cfg)
	defer h.close()
	entries, err := h.saved()
	if err != nil || len(entries) != 2 {
		t.Fatalf("history = %+v, %v", entries, err)
	}
	for _, e := range entries {
		if e.ID != "root" && e.ID != "fork" {
			t.Fatalf("exposed worker in project list: %+v", e)
		}
	}
	_, _, err = h.session(context.Background(), "", "task", draftOptions{})
	var owner agentParent
	if !errors.As(err, &owner) || owner != "root" {
		t.Fatalf("child opened as independent runtime: %v", err)
	}
}

func TestInspectorKeepsParentVisibleAndRoutesAgentResults(t *testing.T) {
	a, root, callbacks := testApp(t)
	commands := make(chan protocol.Command, 1)
	root.client = &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		commands <- cmd
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}}
	root.title, root.view.Busy, root.submitted = "检查项目结构", true, true
	root.draft = "留在主会话的草稿"
	child := protocol.ChildSession{SessionID: "map", ParentSessionID: "s", Name: "MapReduce", Title: "检查 MapReduce 插件目录", ParentCallID: "map-call", Run: protocol.RunView{ID: "original", Status: "succeeded"}, Capabilities: protocol.AgentCapabilities{Continue: true}}
	root.children = []protocol.ChildSession{child, {SessionID: "raft", Name: "Raft", Title: "检查 Raft 日志", Run: protocol.RunView{Status: "running"}}}
	report := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "final_result", AgentID: "map", Name: "MapReduce", Outcome: "succeeded", Text: "插件位于项目的源代码目录。"})
	root.view.Transcript = []protocol.TranscriptItem{
		{ID: "q", Kind: "user", Text: "帮我检查项目结构"},
		{ID: "one", Kind: "tool", Tool: "SpawnAgent", CallID: "map-call", Status: "success"},
		{ID: "two", Kind: "tool", Tool: "SpawnAgent", Status: "success", Text: `{"agent_id":"raft"}`},
		{ID: "result", Kind: "collaboration", Text: report},
		{ID: "wait", Kind: "tool", Tool: "WaitAgent", Status: "running", Args: []byte("{}")},
	}
	root.view.PendingInputs = []protocol.InputView{{Text: report, Strategy: protocol.InputMessage}}
	childCommands := make(chan protocol.Command, 1)
	workerClient := &fakeRuntime{submit: func(cmd protocol.Command) (protocol.Receipt, error) {
		childCommands <- cmd
		if cmd.ExpectedRunID == "outdated" {
			return protocol.Receipt{}, protocol.ErrStaleRun
		}
		return protocol.Receipt{Status: protocol.ReceiptApplied}, nil
	}}
	worker := &conversation{rootID: "s", client: workerClient, connected: true, expanded: make(map[string]bool), title: "检查 MapReduce 插件目录"}
	syncText := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", AgentID: "s", Text: "背景同步正文"})
	worker.view = protocol.SessionView{SessionID: "map", RunID: "original", Settings: root.view.Settings, Transcript: []protocol.TranscriptItem{
		{ID: "mail", Kind: "collaboration", Text: syncText},
		{ID: "reply", Kind: "assistant", Text: "子会话中的完整回答。"},
	}}
	a.conversations["map"] = worker
	tt := ui.NewTester(a.view, 1420, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, worker.title); ok || tt.HasText(report) || tt.HasText("等待处理 · "+shorten(report, 100)) || tt.HasText("等待子任务…") {
		t.Fatal("worker data appeared in main chat or project list")
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, "发送消息"); ok {
		t.Fatal("running session retained a send arrow")
	}
	click("2 个子智能体")
	click("MapReduce")
	if a.active() != root || tt.HasText("背景同步正文") || tt.HasText("协作消息") || !tt.HasText("子会话中的完整回答。") || !tt.HasText("帮我检查项目结构") {
		t.Fatal("child did not open alongside parent chat")
	}
	click("子会话消息")
	tt.Type("继续检查目录")
	click("发送给 MapReduce")
	applyCallback(t, callbacks)
	cmd := <-childCommands
	if cmd.SessionID != "map" || cmd.ExpectedRunID != "original" || cmd.Input.Strategy != protocol.InputSteer || cmd.Input.Text != "继续检查目录" || worker.draft != "" {
		t.Fatalf("incorrect continuation from inspector: %+v", cmd)
	}
	root.children[0].Run = protocol.RunView{ID: "outdated", Status: "running"}
	root.children[0].Capabilities = protocol.AgentCapabilities{Send: true}
	worker.view.RunID, worker.view.Busy = "outdated", true
	tt.Frame()
	click("子会话消息")
	tt.Type("先看入口")
	tt.Key(0, ui.KeyEnter)
	applyCallback(t, callbacks)
	stale := <-childCommands
	if worker.pending != nil || worker.draft != "先看入口" {
		t.Fatal("stale run blocked editing or discarded the draft")
	}
	root.children[0].Run.ID = "continued"
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTextField, "子会话消息"); ok {
		t.Fatal("child composer used another run's capabilities")
	}
	a.send(worker)
	if worker.sending || worker.pending != nil {
		t.Fatal("submitted input while the child view was stale")
	}
	worker.view.RunID = "continued"
	tt.Frame()
	click("发送给 MapReduce")
	applyCallback(t, callbacks)
	cmd = <-childCommands
	if cmd.SessionID != "map" || cmd.ID == stale.ID || cmd.ExpectedRunID != "continued" || cmd.Input.Text != "先看入口" || worker.draft != "" {
		t.Fatalf("incorrect steering from inspector: %+v", cmd)
	}
	if root.draft != "留在主会话的草稿" || len(commands) != 0 || a.active() != root {
		t.Fatal("child input was routed to the parent")
	}
	root.children[0].Capabilities = protocol.AgentCapabilities{Observe: true}
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTextField, "子会话消息"); ok {
		t.Fatal("read-only child offered a text field")
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, "发送给 MapReduce"); ok {
		t.Fatal("read-only child offered message submission")
	}
	if tt.HasText("MapReduce · 已完成") || tt.HasText("已完成") {
		t.Fatal("successful child produced a redundant status row")
	}
	click("子智能体报告")
	click("MapReduce 的报告")
	if !tt.HasText("插件位于项目的源代码目录。") || tt.HasText(report) {
		t.Fatal("report did not display as separate content")
	}
	tt.SetScale(2)
	captureSidebar(t, tt, "inspector-agents.png")
	if len(root.inspector.pages) != 1 {
		t.Fatal("report duplicated the agent tab")
	}
	root.children[0].Run.Status = "succeeded"
	root.children[0].Capabilities = protocol.AgentCapabilities{Continue: true}
	tt.Frame()
	click("子会话消息")
	tt.Type("从报告直接继续")
	tt.Key(0, ui.KeyEnter)
	applyCallback(t, callbacks)
	if cmd := <-childCommands; cmd.SessionID != "map" || cmd.ExpectedRunID != "continued" || cmd.Input.Text != "从报告直接继续" {
		t.Fatalf("report reply went to wrong session: %+v", cmd)
	}
	tt.Frame()
	if !tt.HasText("子会话中的完整回答。") || root.draft != "留在主会话的草稿" {
		t.Fatal("reply from a report did not follow the live child")
	}
	click("收起右栏")
	if tt.HasText("子会话中的完整回答。") || tt.HasText("插件位于项目的源代码目录。") {
		t.Fatal("child content leaked into main transcript")
	}
	click("停止执行")
	applyCallback(t, callbacks)
	if cmd := <-commands; cmd.Type != protocol.CommandInterrupt || cmd.SessionID != "s" {
		t.Fatal(cmd)
	}
	root.view.Busy = false
	root.children[1].Run.Status = "succeeded"
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleButton, "发送消息"); !ok {
		t.Fatal("send arrow did not return")
	}
	a.openSession("", "map")
	if a.active() != root || !root.inspector.open || root.inspector.current.agent.id != "map" {
		t.Fatal("direct child navigation replaced the main conversation")
	}
}

func TestInspectorFilesChangesAndMissingReads(t *testing.T) {
	a, root, callbacks := testApp(t)
	root.client = &fakeRuntime{}
	dir := t.TempDir()
	root.view.Settings.Workspace = dir
	if err := os.Mkdir(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "missing")
	recorded := "Read failed after 8ms: lstat " + absent + ": no such file or directory"
	root.view.Transcript = []protocol.TranscriptItem{
		{ID: "one", Kind: "tool", Tool: "Read", Status: "error", Args: []byte(`{"file_path":"missing/a.go"}`), Text: recorded},
		{ID: "two", Kind: "tool", Tool: "Read", Status: "error", Args: []byte(`{"file_path":"missing/b.go"}`), Text: recorded},
		{ID: "edit", Kind: "tool", Tool: "Edit", Status: "success", Args: []byte(`{"file_path":"src/main.go","edits":[{"old_text":"package app","new_text":"package main"}]}`)},
	}
	root.view.Transcript = append(root.view.Transcript, protocol.TranscriptItem{ID: "answer", Kind: "assistant", Text: "[打开源文件](src/main.go#L1) 和 [项目说明](README.md)"})
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("项目说明正文"), 0600); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	load := func(label string) { t.Helper(); click(label); applyCallback(t, callbacks); tt.Frame() }
	if tt.HasText(recorded) || tt.HasText("读取 a.go · 路径不存在") {
		t.Fatal("individual failures visible before expansion")
	}
	click("2 次读取失败 · 找不到 missing")
	click("读取 a.go · 路径不存在")
	if !tt.HasText(recorded) {
		t.Fatal("lost the original failure details")
	}
	if _, ok := tt.AccessByRole(ui.RoleGroup, "右栏"); ok {
		t.Fatal("panel open before an object is selected")
	}
	load("打开源文件")
	if !tt.HasText("package main") {
		t.Fatal("file preview missing")
	}
	load("项目说明")
	click("切换到 main.go")
	if !tt.HasText("package main") || tt.HasText("项目说明正文") {
		t.Fatal("wrong active file")
	}
	click("已编辑 main.go")
	click("已编辑 main.go")
	if !tt.HasText("+ package main") || !tt.HasText("− package app") {
		t.Fatal("recorded changes missing")
	}
	load("查看当前文件")
	if len(root.inspector.pages) != 2 {
		t.Fatal("reopening the file duplicated its tab", len(root.inspector.pages))
	}
	click("关闭 main.go")
	if !tt.HasText("项目说明正文") {
		t.Fatal("did not select remaining tab")
	}
	click("关闭 README.md")
	if root.inspector.open || len(root.inspector.pages) != 0 {
		t.Fatal("closing last tab left a blank panel")
	}
	if _, err := loadInspectorFile(dir, "../elsewhere"); err == nil {
		t.Fatal("opened a file outside project")
	}
}

func TestFailedTurnSummary(t *testing.T) {
	a, root, _ := testApp(t)
	root.view.Transcript = []protocol.TranscriptItem{{ID: "summary", Kind: "turn_summary", Status: "error", DurationMs: 500}}
	tt := ui.NewTester(a.view, 1000, 650)
	if !tt.HasText("失败 · 0.5 秒") || tt.HasText("已完成 · 0.5 秒") {
		t.Fatal("failed provider request displayed as successful")
	}
}

type reportDirectory struct {
	protocol.SessionDirectory
	client protocol.SessionClient
}

func (dir reportDirectory) OpenReader(context.Context, protocol.SessionID) (protocol.SessionClient, error) {
	return dir.client, nil
}

func TestUnopenedAgentReportAttachesComposer(t *testing.T) {
	a, root, callbacks := testApp(t)
	worker := &fakeRuntime{view: protocol.SessionView{SessionID: "map", RunID: "latest"}}
	root.client = &fakeRuntime{}
	root.directory = reportDirectory{client: worker}
	root.children = []protocol.ChildSession{{SessionID: "map", Name: "MapReduce", Run: protocol.RunView{ID: "latest", Status: "succeeded"}, Capabilities: protocol.AgentCapabilities{Continue: true}}}
	for _, body := range []string{"先前的报告", "点开的消息"} {
		a.inspectAgent(root, agentDetail{id: "map", name: "MapReduce", report: body, isReport: true})
	}
	applyCallback(t, callbacks)
	tt := ui.NewTester(a.view, 1280, 800)
	if !tt.HasText("点开的消息") || tt.HasText("查看子会话") || len(root.inspector.pages) != 1 {
		t.Fatal("opening a second report lost its selection or required an intermediate step")
	}
	if _, ok := tt.AccessByRole(ui.RoleTextField, "子会话消息"); ok {
		t.Fatal("report showed a composer before the child connected")
	}
	a.conversations["map"].connected = true
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleButton, "发送给 MapReduce"); !ok {
		t.Fatal("no composer on first open of a saved report")
	}
	root.view.Closing = true
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTextField, "子会话消息"); ok || !tt.HasText("点开的消息") {
		t.Fatal("closing the parent should leave a readable report without its composer")
	}
}

func TestRoutineMailboxEventsStayHiddenInExecutionHistory(t *testing.T) {
	a, root, _ := testApp(t)
	mail := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", AgentID: "worker", Text: "private sync"})
	final := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "final_result", AgentID: "worker", Name: "worker", Outcome: "succeeded", Text: "final report"})
	args := []byte(`{"agent_id":"worker","text":"private sync"}`)
	// Put some traffic on the older page and the remainder in the live window.
	root.history = []protocol.TranscriptItem{
		{ID: "question", Kind: "user", TurnID: "t", Text: "actual question"},
		{ID: "out", Kind: "tool", Tool: "SendMessage", Status: "success", TurnID: "t", Args: args},
		{ID: "read", Kind: "tool", Tool: "Read", Status: "success", TurnID: "t"},
	}
	root.view.Transcript = []protocol.TranscriptItem{
		{ID: "in", Kind: "collaboration", Text: mail},
		{ID: "sending", Kind: "tool", Tool: "SendMessage", Status: "running", TurnID: "t", Args: args},
		{ID: "result", Kind: "collaboration", Text: final},
		{ID: "failed", Kind: "tool", Tool: "SendMessage", Status: "error", TurnID: "t", Args: args, Text: "private sync"},
	}
	root.expanded["activity:t/question"] = true
	tt := ui.NewTester(a.view, 1200, 800)
	for _, label := range []string{"actual question", "执行记录 · 2 项", "worker 的报告", "发送协作消息 · 失败"} {
		if !tt.HasText(label) {
			t.Errorf("missing %q in expanded conversation", label)
		}
	}
	for _, label := range []string{"private sync", mail, string(args), "发送协作消息 · 成功", "发送协作消息 · 进行中"} {
		if tt.HasText(label) {
			t.Errorf("routine delivery visible as %q", label)
		}
	}
}
