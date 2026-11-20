package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/config"

	"github.com/egoist/mygo/ui"
)

func TestProjectPageEditsAndReturnsToConversation(t *testing.T) {
	a, conv, callbacks := testApp(t)
	project := t.TempDir()
	const draft = "原来聊天中还没发送的问题"
	conv.draft = draft
	// A malformed fixture exercises the extension tab's asynchronous error
	// state before discovery can read any machine-local skills or trust.
	a.configPath = filepath.Join(t.TempDir(), "invalid-config.json")
	if err := os.WriteFile(a.configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	a.openProjectPage(project, false)
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(labels ...string) {
		t.Helper()
		for _, label := range labels {
			if err := tt.Click(label); err != nil {
				t.Fatal(err)
			}
		}
	}
	edit := func(label, text string) {
		t.Helper()
		click(label)
		tt.Key(ui.Cmd, ui.KeyA)
		tt.Type(text)
	}
	assertField := func(label, want string) {
		t.Helper()
		info, ok := tt.Access(label)
		if !ok || info.Value != want {
			t.Fatalf("%s: got %q, want %q", label, info.Value, want)
		}
	}
	const name = "文档工作台"
	edit("显示名称输入框", name)
	click("保存名称")
	store, err := loadProjectStore(a.projects.path)
	if err != nil || store.get(project).Name != name {
		t.Fatalf("failed to persist display name: %v", err)
	}
	click("项目指令")
	const rootText = "# 开发约定\n\n提交前运行 go test ./...。\n"
	const localText = "# 项目测试\n\n用 make check 验证格式和单元测试。\n"
	edit("项目指令输入框", rootText)
	click(".ccdp/AGENTS.md")
	assertField("项目指令输入框", "")
	edit("项目指令输入框", localText)
	click("保存指令")
	click("AGENTS.md")
	assertField("项目指令输入框", rootText)
	click("返回对话", "继续编辑")
	assertField("项目指令输入框", rootText)
	tt.Key(ui.Cmd, ui.KeyS)
	for file, want := range map[string]string{"AGENTS.md": rootText, ".ccdp/AGENTS.md": localText} {
		data, err := os.ReadFile(filepath.Join(project, file))
		if err != nil || string(data) != want {
			t.Fatalf("saved %s: %q %v", file, data, err)
		}
	}
	capture := func(name string) {
		t.Helper()
		dir := os.Getenv("CCDP_DESKTOP_SCREENSHOTS")
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	capture("project-details")

	click("扩展")
	if !tt.HasText("正在读取项目扩展…") {
		t.Fatal("extension tab did not start loading")
	}
	applyCallback(t, callbacks)
	tt.Frame()
	if result := a.projectPage.extensions; result == nil || result.problem == "" || !tt.HasText(result.problem) {
		t.Fatal("extension tab did not display the configuration error")
	}
	profile := t.TempDir()
	userSkills := filepath.Join(profile, "skills")
	for path, content := range map[string]string{
		filepath.Join(profile, "config.json"):                           `{"mcp_servers":{"docs-search":{"command":"fixture-only"}}}`,
		filepath.Join(project, ".ccdp", "settings.json"):                `{"mcp_servers":{"project-index":{"command":"fixture-only"}}}`,
		filepath.Join(project, ".ccdp", "skills", "review", "SKILL.md"): "---\ndescription: 检查文档中的命令、示例和术语。\n---\nReview the documentation.",
		filepath.Join(userSkills, "release-notes", "SKILL.md"):          "---\ndescription: 根据改动整理版本说明。\n---\nPrepare release notes.",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Fill the same view with a catalog discovered under explicit fixture roots.
	a.projectPage.extensions = readProjectExtensions(project, filepath.Join(profile, "config.json"), userSkills,
		config.NewTrustStore(filepath.Join(profile, "trust.json")))
	tt.Frame()
	for _, label := range []string{"docs-search", "project-index", "项目 · 待信任", "release-notes", "review"} {
		if !tt.HasText(label) {
			t.Fatalf("project extension view did not show %q: %v", label, tt.Texts())
		}
	}
	capture("project-extensions")
	click("项目指令")
	edit("项目指令输入框", "未保存的试改")
	click("返回对话", "放弃更改")
	assertField("消息", draft)
	if a.active() != conv || a.projectPage != nil || conv.draft != draft {
		t.Fatal("returning from the editor changed the active conversation or draft")
	}
	if got, err := os.ReadFile(filepath.Join(project, "AGENTS.md")); err != nil || string(got) != rootText {
		t.Fatal("discarded instruction text changed the saved file")
	}
}
