package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/protocol"

	"github.com/egoist/mygo/ui"
)

func TestProjectExtensionsRespectTrustAndScope(t *testing.T) {
	root := t.TempDir()
	user := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(user, "profiles", "test.json")
	userSkills := filepath.Join(user, "skills")
	projectDir := config.ProjectSettingsDir(root)
	trust := config.NewTrustStore(filepath.Join(user, "trust.json"))
	write(cfgPath, `{"mcp_servers":{"shared":{"command":"secret-command","env":{"KEY":"redacted-fixture-key"}}}}`)
	write(filepath.Join(projectDir, "settings.json"), `{"mcp_servers":{"replaced":{"command":"fixture-command"}},"hooks":{"PostToolUse":["secret-command"]}}`)
	write(filepath.Join(projectDir, "settings.local.json"), `{"mcp_servers":{"local":{"command":"another-command"}}}`)
	write(filepath.Join(userSkills, "publish", "SKILL.md"), "User skill body")
	write(filepath.Join(userSkills, "review", "SKILL.md"), "Shadowed review skill")
	write(filepath.Join(projectDir, "skills", "review", "SKILL.md"), "---\ndescription: Review the project\n---\nPrivate skill body")

	read := func() *projectExtensions {
		t.Helper()
		p := readProjectExtensions(root, cfgPath, userSkills, trust)
		if p.problem != "" || p.skillsProblem != "" {
			t.Fatalf("discovery: %s %s", p.problem, p.skillsProblem)
		}
		return p
	}
	p := read()
	if len(p.skills) != 2 || p.skills[0].name != "publish" || p.skills[0].source != config.SourceUserFile ||
		p.skills[1].name != "review" || p.skills[1].source != config.SourceProject || p.skills[1].detail != "Review the project" {
		t.Fatalf("skill precedence: %+v", p.skills)
	}
	if len(p.servers) != 3 || p.servers[0].name != "exa" || p.servers[0].source != config.SourceDefault || p.servers[1].name != "shared" || p.servers[1].pendingTrust ||
		p.servers[2].name != "local" || !p.servers[2].pendingTrust || p.servers[2].source != config.SourceProjectLocal {
		t.Fatalf("untrusted project overlay: %+v", p.servers)
	}
	if len(p.hooks) != 1 || !p.hooks[0].pendingTrust || p.hooks[0].source != config.SourceProject {
		t.Fatalf("hook provenance: %+v", p.hooks)
	}
	if _, err := os.Stat(trust.Path); !os.IsNotExist(err) {
		t.Fatalf("discovery modified trust store: %v", err)
	}
	for _, secret := range []string{"redacted-fixture-key", "secret-command", "Private skill body"} {
		if strings.Contains(fmt.Sprint(p), secret) {
			t.Fatal("display metadata contains a command, secret or skill body")
		}
	}
	project, err := config.LoadProjectSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.AuthorizeProject(root, project); err != nil {
		t.Fatal(err)
	}
	p = read()
	if len(p.servers) != 1 || p.servers[0].name != "local" || p.servers[0].pendingTrust || p.hooks[0].pendingTrust {
		t.Fatalf("trusted project override: %+v", p)
	}
	userCfg, err := config.LoadFrom(cfgPath)
	installed, _ := configuredExtensions(root, userCfg, err, trust, true)
	if len(installed.servers) != 3 || installed.servers[0].shadowed || !installed.servers[1].shadowed || !installed.servers[2].shadowed {
		t.Fatalf("installation list lost overridden user services: %+v", installed.servers)
	}
}

func TestProjectExtensionsEmptyAndUnreadable(t *testing.T) {
	root, user := t.TempDir(), t.TempDir()
	cfg := filepath.Join(user, "config.json")
	read := func() *projectExtensions {
		return readProjectExtensions(root, cfg, filepath.Join(user, "skills"), config.NewTrustStore(filepath.Join(user, "trust.json")))
	}
	p := read()
	if p.problem != "" || p.skillsProblem != "" || len(p.skills)+len(p.hooks)+len(p.tools) != 0 || len(p.servers) != 1 || p.servers[0].name != "exa" || p.servers[0].source != config.SourceDefault {
		t.Fatalf("unexpected entries in an empty project: %+v", p)
	}
	if err := os.WriteFile(cfg, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if read().problem == "" {
		t.Fatal("malformed configuration was presented as an empty project")
	}
}

func TestProjectExtensionsViewUsesLiveCatalog(t *testing.T) {
	var p *projectExtensions
	var catalog *protocol.CatalogSnapshot
	refreshed := false
	tester := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Children(func() {
			projectExtensionsView(c, c.Theme().Surface, p, catalog, "测试会话", func() { refreshed = true })
		})
	}, 800, 620)
	if !tester.HasText("正在读取项目扩展…") {
		t.Fatal("missing loading state")
	}
	p = &projectExtensions{servers: []projectExtension{{name: "example", source: config.SourceProject, pendingTrust: true}}}
	tester.Frame()
	if !tester.HasText("example") || !tester.HasText("项目 · 待信任") || !tester.HasText("此项目的会话连接后可查看工具列表。") {
		t.Fatalf("configured server without a session: %v", tester.Texts())
	}
	catalog = &protocol.CatalogSnapshot{Tools: []protocol.ToolSnapshot{{ID: "Read"}}}
	tester.Frame()
	if !tester.HasText("会话尚未发布 MCP 工具。") {
		t.Fatal("built-in tool was counted as an MCP tool")
	}
	catalog.Tools = append(catalog.Tools, protocol.ToolSnapshot{ID: "mcp__example__lookup"})
	tester.Frame()
	if !tester.HasText("mcp__example__lookup") {
		t.Fatal("missing published tool")
	}
	if err := tester.Click("刷新项目扩展"); err != nil || !refreshed {
		t.Fatalf("refresh failed: %v", err)
	}
}
