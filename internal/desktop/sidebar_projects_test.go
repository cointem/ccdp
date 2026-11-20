package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func TestSidebarProjectMenuAndSearch(t *testing.T) {
	a, conv, callbacks := testApp(t)
	projectDir := func(parent string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), parent, "app")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		return projectKey(path)
	}
	frontend, server := projectDir("frontend"), projectDir("server")
	if err := a.projects.update(frontend, func(meta *projectMetadata) { meta.Name = "桌面客户端" }); err != nil {
		t.Fatal(err)
	}
	if err := a.projects.update(server, func(meta *projectMetadata) { meta.Name = "服务端" }); err != nil {
		t.Fatal(err)
	}
	a.workspace = frontend
	conv.view.Settings.Workspace = frontend
	conv.title, conv.client = "检查发布流程", &fakeRuntime{}
	conv.view.HistoryLength = 1
	a.saved = []savedSession{{ID: "history", Title: "验证接口文档", Workspace: frontend, UpdatedAt: time.Now()}}

	tt := ui.NewTester(a.view, 1180, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	choose := func(item string) {
		t.Helper()
		if err := tt.ChooseMenuItem(item); err != nil {
			t.Fatal(err)
		}
	}
	for _, count := range []string{"0", "2"} {
		if slices.Contains(tt.Texts(), count) {
			t.Fatalf("sidebar displays a project session count %q", count)
		}
	}
	click("项目菜单 · 服务端")
	for _, item := range []string{"编辑项目", "当前项目的扩展", "置顶项目"} {
		if !slices.Contains(tt.Menu(), item) {
			t.Fatalf("empty project menu missing %q: %v", item, tt.Menu())
		}
	}
	choose("置顶项目")
	groups := a.sidebarProjects(map[string]string{})
	if len(groups) != 2 || groups[0].path != server || groups[1].path != frontend {
		t.Fatal("pinning the empty project should place it above the two-session project")
	}
	click("项目菜单 · 桌面客户端")
	captureSidebar(t, tt, "projects.png")
	choose("折叠项目")
	if _, ok := tt.AccessByRole(ui.RoleButton, "验证接口文档"); ok || !a.projects.get(frontend).Collapsed {
		t.Fatal("collapsed sidebar still exposes historical chat")
	}
	click("搜索项目或会话")
	tt.Type("接口文档")
	if _, ok := tt.AccessByRole(ui.RoleButton, "验证接口文档"); !ok {
		t.Fatal("a matching chat in a collapsed group was hidden from search")
	}
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("服务端")
	if _, ok := tt.Find("在 服务端 新建对话"); !ok {
		t.Fatal("an empty project's display name was not searchable")
	}
	click("在 服务端 新建对话")
	if a.workspace != server || a.active().view.Settings.Workspace != server || a.active().client != nil {
		t.Fatal("new chat did not start as a draft inside the selected project")
	}
	a.configPath = filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(a.configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	click("搜索项目或会话")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Key(0, ui.KeyBackspace)
	click("项目菜单 · 桌面客户端")
	choose("当前项目的扩展")
	if a.projectPage == nil || a.projectPage.path != frontend || a.projectPage.tab != projectExtensionsTab {
		t.Fatal("contextual extensions opened a different project")
	}
	applyCallback(t, callbacks)
	tt.Frame()
	click("返回对话")
	click("项目菜单 · 桌面客户端")
	choose("编辑项目")
	if a.projectPage == nil || a.projectPage.path != frontend || a.projectPage.tab != projectDetailsTab {
		t.Fatal("edit project menu did not navigate to the selected project")
	}
	if !tt.HasText("2 个会话") {
		t.Fatal("project details lost their session count")
	}
	click("显示名称输入框")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("尚未保存的名称")
	click("自定义")
	click("继续编辑")
	name, ok := tt.Access("显示名称输入框")
	if !ok || name.Value != "尚未保存的名称" {
		t.Fatal("canceling navigation lost the unsaved project name")
	}
	click("自定义")
	click("放弃更改")
	if a.projectPage != nil || a.pluginsPage == nil || a.pluginsPage.path != frontend {
		t.Fatal("discarding project edits did not navigate to customization for the edited project")
	}
	if a.projects.get(frontend).Name != "桌面客户端" {
		t.Fatal("discarding project edits persisted the draft name")
	}
	applyCallback(t, callbacks)
}

func TestSidebarLegacyProjectsAndSymlinks(t *testing.T) {
	a, conv, _ := testApp(t)
	dir := t.TempDir()
	one := filepath.Join(dir, "one", "app")
	two := filepath.Join(dir, "two", "app")
	for _, path := range []string{one, two} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(dir, "link")
	if err := os.Symlink(one, alias); err != nil {
		t.Fatal(err)
	}
	conv.view.Settings.Workspace = alias
	conv.draft = "保留项目里的草稿"
	a.saved = []savedSession{
		{ID: "one", Title: "Published session", Workspace: one},
		{ID: "two", Title: "Another session", Workspace: two},
	}
	groups := a.sidebarProjects(map[string]string{})
	if len(groups) != 2 {
		t.Fatalf("two workspaces with distinct paths and identical basenames produced %d groups", len(groups))
	}
	for _, g := range groups {
		if !g.duplicate {
			t.Fatal("same-name projects from different paths need separate labels")
		}
	}
}

func writeNavigationSkill(t *testing.T, parent, name, description string) {
	t.Helper()
	dir := filepath.Join(parent, ".ccdp", "skills", name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: " + description + "\n---\n" + description
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCustomizationNavigationRail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a, conv, callbacks := testApp(t)
	a.configPath = filepath.Join(t.TempDir(), "config.json")
	a.workspace = filepath.Join(projectKey(t.TempDir()), "6.5840")
	conv.view.Settings.Workspace = a.workspace
	conv.draft = "继续检查项目改动"
	writeNavigationSkill(t, a.workspace, "review-guide", "按项目约定检查补丁并标出需要补测的改动。")
	writeNavigationSkill(t, a.workspace, "test-plan", "梳理功能边界、测试步骤与验收条件。")
	writeNavigationSkill(t, home, "release-notes", "整理版本更新中的新增功能与重要修复。")
	writeNavigationSkill(t, home, "api-spec", "根据接口定义生成可运行的请求示例。")
	if err := a.projects.update(a.workspace, nil); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	railButton := func(label string, current ui.Current) ui.AccessInfo {
		t.Helper()
		button, ok := tt.AccessByRole(ui.RoleButton, label)
		if !ok || button.Offscreen || button.Disabled || button.Current != current {
			t.Fatalf("unexpected navigation state for %q: %+v", label, button)
		}
		return button
	}
	railButton("对话", ui.CurrentPage)
	customize := railButton("自定义", ui.CurrentNone)
	search, ok := tt.Access("搜索项目或会话")
	if !ok || customize.Bounds.X+customize.Bounds.W > search.Bounds.X {
		t.Fatalf("customization does not precede the project sidebar: button=%+v search=%+v", customize.Bounds, search.Bounds)
	}
	click("收起侧栏")
	if _, ok := tt.Find("搜索项目或会话"); ok {
		t.Fatal("project search remained visible when the sidebar collapsed")
	}
	railButton("自定义", ui.CurrentNone)
	click("自定义")
	applyCallback(t, callbacks)
	tt.Frame()
	railButton("自定义", ui.CurrentPage)
	railButton("对话", ui.CurrentNone)
	sub := railButton("插件", ui.CurrentPage)
	if sub.Bounds.X < customize.Bounds.X+customize.Bounds.W {
		t.Fatal("plugins must be in the second navigation column")
	}
	skills := railButton("技能", ui.CurrentNone)
	if sub.Bounds.X != skills.Bounds.X || sub.Bounds.Y+sub.Bounds.H > skills.Bounds.Y {
		t.Fatal("plugins and skills are not stacked in a common navigation column")
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, "安装本地插件"); !ok || a.projectPage != nil {
		t.Fatal("customization did not open on the local plugin page")
	}
	if !tt.HasText("exa") || !tt.HasText("MCP 服务 · 默认安装") || tt.HasText("还没有安装插件") {
		t.Fatalf("plugins page with only the default MCP installed: %v", tt.Texts())
	}
	click("技能")
	railButton("技能", ui.CurrentPage)
	railButton("插件", ui.CurrentNone)
	if !tt.HasText("当前项目") || tt.HasText("6.5840") {
		t.Fatal("the skill scope filter should have an explicit label even for a numeric workspace name")
	}
	search, ok = tt.Access("搜索技能")
	if !ok || search.Bounds.X < skills.Bounds.X+skills.Bounds.W {
		t.Fatal("skill search must be in the content pane to the right of the second column")
	}
	for _, name := range []string{"review-guide", "test-plan", "release-notes", "api-spec"} {
		if !tt.HasText(name) {
			t.Fatalf("missing discovered skill %q", name)
		}
	}
	if add, ok := tt.Access("添加技能"); !ok || add.Disabled || add.Offscreen {
		t.Fatal("skill page has no install action")
	}
	click("添加技能")
	for _, target := range []string{"添加到个人技能", "添加到当前项目"} {
		if !slices.Contains(tt.Menu(), target) {
			t.Fatalf("skill install menu missing %q", target)
		}
	}
	tt.Key(0, ui.KeyEscape)
	captureSidebar(t, tt, "customization.png")
	click("搜索技能")
	tt.Type("REVIEW")
	if !tt.HasText("review-guide") || tt.HasText("release-notes") || tt.HasText("test-plan") {
		t.Fatal("skill search did not narrow the list by name")
	}
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Key(0, ui.KeyBackspace)
	click("个人")
	if !tt.HasText("release-notes") || tt.HasText("review-guide") {
		t.Fatal("personal scope includes the wrong skills")
	}
	click("当前项目")
	if !tt.HasText("test-plan") || tt.HasText("api-spec") {
		t.Fatal("project scope includes the wrong skills")
	}
	click("搜索技能")
	tt.Type("验收条件")
	if !tt.HasText("test-plan") || tt.HasText("review-guide") {
		t.Fatal("searching skill descriptions did not respect the selected scope")
	}
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Key(0, ui.KeyBackspace)
	click("全部")
	if !tt.HasText("api-spec") || !tt.HasText("review-guide") {
		t.Fatal("all scopes did not restore the complete list")
	}
	click("review-guide")
	if !tt.HasText(filepath.Join("review-guide", "SKILL.md")) {
		t.Fatal("skill details did not show the source file")
	}
	click("关闭")
	tt.SetSize(780, 580)
	railButton("自定义", ui.CurrentPage)
	click("对话")
	railButton("对话", ui.CurrentPage)
	draft, ok := tt.Access("消息")
	if !ok || draft.Value != conv.draft || a.active() != conv {
		t.Fatal("returning from customization lost the current chat or its draft")
	}
	if _, ok := tt.Find("搜索项目或会话"); ok {
		t.Fatal("switching views changed the collapsed sidebar preference")
	}
}

func TestCustomizationWithoutWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNavigationSkill(t, home, "personal-guide", "我的开发约定。")
	source := t.TempDir()
	for rel, data := range map[string]string{
		".codex-plugin/plugin.json":  `{"name":"docs-kit","interface":{"displayName":"文档工具箱"}}`,
		"skills/link-check/SKILL.md": "---\ndescription: 检查文档链接。\n---\nVerify linked files.",
	} {
		path := filepath.Join(source, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := installCustomPlugin(filepath.Join(home, ".ccdp", "skills"), source); err != nil {
		t.Fatal(err)
	}
	a, conv, callbacks := testApp(t)
	a.configPath = filepath.Join(home, ".ccdp", "config.json")
	a.workspace, conv.view.Settings.Workspace = "", ""
	tt := ui.NewTester(a.view, 780, 580)
	if err := tt.Click("自定义"); err != nil {
		t.Fatal(err)
	}
	button, ok := tt.AccessByRole(ui.RoleButton, "安装本地插件")
	if !ok || button.Offscreen || button.Disabled {
		t.Fatal("the plugin page requires a project for a user installation")
	}
	applyCallback(t, callbacks)
	tt.Frame()
	if !tt.HasText("文档工具箱") {
		t.Fatal("the local plugin was not listed without a workspace")
	}
	captureSidebar(t, tt, "plugins.png")
	if _, ok := tt.AccessByRole(ui.RoleButton, "MCP"); !ok {
		t.Fatal("MCP needs a dedicated navigation entry")
	}
	if !tt.HasText("exa") || !tt.HasText("MCP 服务 · 默认安装") {
		t.Fatalf("default MCP missing without a workspace or session: %v", tt.Texts())
	}
	if err := os.WriteFile(a.configPath, []byte(`{"mcp_servers":{"docs-kit":{"command":"private-command"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("刷新插件"); err != nil {
		t.Fatal(err)
	}
	applyCallback(t, callbacks)
	tt.Frame()
	if !tt.HasText("docs-kit") || !tt.HasText("MCP 服务 · 用户") || !tt.HasText("exa") || tt.HasText("private-command") {
		t.Fatalf("refreshed MCP list: %v", tt.Texts())
	}
	if err := tt.Click("搜索插件"); err != nil {
		t.Fatal(err)
	}
	tt.Type("docs")
	if tt.HasText("MCP 服务 · 默认安装") || !tt.HasText("MCP 服务 · 用户") {
		t.Fatalf("filtered MCP list: %v", tt.Texts())
	}
	if err := tt.Click("docs-kit"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText(a.configPath) || !tt.HasText("配置 MCP") || !tt.HasText("卸载 MCP") || tt.HasText("卸载插件") || tt.HasText("private-command") {
		t.Fatalf("MCP details for a server sharing a bundle ID: %v", tt.Texts())
	}
	if err := tt.Click("返回插件列表"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("文档工具箱"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("包含的技能") || !tt.HasText("link-check") {
		t.Fatal("installed plugin details are missing the provided skill")
	}
	if err := tt.Click("技能"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("personal-guide") || !tt.HasText("link-check") {
		t.Fatal("standalone and plugin-provided skills should both be available without a workspace")
	}
	if err := tt.Click("对话"); err != nil {
		t.Fatal(err)
	}
	if a.active() != conv {
		t.Fatal("customization replaced the current chat")
	}
}

func captureSidebar(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("CCDP_DESKTOP_SCREENSHOTS")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}
