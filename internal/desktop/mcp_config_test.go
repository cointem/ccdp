package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"

	"github.com/egoist/mygo/ui"
)

func TestMCPManagementLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, conv, callbacks := testApp(t)
	a.workspace, conv.view.Settings.Workspace = "", ""
	a.configPath = filepath.Join(t.TempDir(), "config.json")
	tt := ui.NewTester(a.view, 1000, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	refresh := func() { t.Helper(); applyCallback(t, callbacks); tt.Frame() }
	read := func() config.Config {
		t.Helper()
		cfg, err := config.LoadFrom(a.configPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	click("自定义")
	refresh()
	click("MCP")
	if tt.HasText("已安装") {
		t.Fatal("MCP page repeats the installation heading")
	}
	count := 0
	for _, item := range tt.AccessAll() {
		if item.Role == ui.RoleButton && item.Name == "exa" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one Exa listing, found %d", count)
	}
	captureSidebar(t, tt, "mcp-category.png")
	click("添加 MCP")
	click("名称输入框")
	tt.Type("notes")
	click("启动命令输入框")
	tt.Type("notes-server")
	click("保存 MCP")
	refresh()
	cfg := read()
	if cfg.MCPServers["notes"].Command != "notes-server" || !cfg.MCPServers["exa"].DefaultInstall {
		t.Fatal("adding a server lost an installation")
	}
	click("notes")
	click("配置 MCP")
	click("启动参数（JSON 数组）输入框")
	tt.Type(`["--stdio"]`)
	click("保存 MCP")
	refresh()
	if args := a.host.settings.Load().MCPServers["notes"].Args; len(args) != 1 || args[0] != "--stdio" {
		t.Fatalf("new session settings after edit: %v", args)
	}
	click("exa")
	click("卸载 MCP")
	captureSidebar(t, tt, "mcp-remove.png")
	click("取消")
	if !read().MCPServers["exa"].DefaultInstall {
		t.Fatal("cancel removed Exa")
	}
	click("卸载 MCP")
	click("确认卸载")
	refresh()
	cfg = read()
	if _, ok := cfg.RuntimeMCPServers()["exa"]; ok || cfg.MCPServers["notes"].Command != "notes-server" {
		t.Fatal("wrong registrations after removal")
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, "exa"); ok {
		t.Fatal("uninstalled server still listed")
	}
	if _, ok := a.host.settings.Load().MCPServers["exa"]; ok {
		t.Fatal("host retained uninstalled server")
	}
	click("notes")
	click("卸载 MCP")
	click("确认卸载")
	refresh()
	if len(read().MCPServers) != 0 {
		t.Fatal("deleting last custom server resurrected default")
	}
	click("重新安装 Exa")
	click("保存 MCP")
	refresh()
	if !read().MCPServers["exa"].DefaultInstall {
		t.Fatal("reinstall lost shipped manifest")
	}
}

func TestMCPProjectEditsUseOwningFile(t *testing.T) {
	dir := t.TempDir()
	personal := filepath.Join(t.TempDir(), "config.json")
	project := filepath.Join(config.ProjectSettingsDir(dir), "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(project), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{personal, project} {
		if err := os.WriteFile(file, []byte(`{"mcp_servers":{"notes":{"command":"old","env":{"keep":"yes"},"future":9007199254740993}},"max_turns":12}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entry := projectExtension{name: "notes", source: config.SourceProjectLocal, path: project}
	open := func() *mcpEditor {
		t.Helper()
		e, err := readMCPEditor(personal, dir, entry, false)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	stale := open()
	e := open()
	e.command = "updated"
	if _, err := e.save(false); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.save(true); err == nil {
		t.Fatal("stale project editor removed changed service")
	}
	b, err := os.ReadFile(project)
	if err != nil || !strings.Contains(string(b), "9007199254740993") || !strings.Contains(string(b), `"keep": "yes"`) {
		t.Fatal("advanced fields lost on edit", err)
	}
	if _, err := open().save(true); err != nil {
		t.Fatal(err)
	}
	proj, err := config.LoadProjectSettings(dir)
	if err != nil || len(proj.MCPServers) != 0 || !proj.IsExplicit("mcp_servers") {
		t.Fatal("project override lost", err)
	}
	user, err := config.LoadFrom(personal)
	if err != nil || user.MCPServers["notes"].Command != "old" {
		t.Fatal("project edit changed personal service", err)
	}
}

func TestMCPAddToExplicitEmptyAndRejectStaleUserEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp_servers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := readMCPEditor(path, "", projectExtension{}, false)
	if err != nil {
		t.Fatal(err)
	}
	e.name, e.transport, e.endpoint = "notes", "http", "https://example.com/mcp"
	if _, err := e.save(false); err == nil {
		t.Fatal("accepted remote server without consent")
	}
	e.network = true
	cfg, err := e.save(false)
	if err != nil || len(cfg.MCPServers) != 1 || cfg.MCPServers["notes"].BaseURL != e.endpoint {
		t.Fatal("adding to explicit empty table enabled defaults", err)
	}
	if _, err := e.save(false); err == nil {
		t.Fatal("stale user editor saved")
	}
}
