package desktop

import (
	"ccdp/internal/mcp"

	"github.com/egoist/mygo/ui"
)

func (a *app) mcpRemovalDialog(c *ui.Context, p *customizationPage) {
	ui.Modal(c, &p.removingMCP, func() {
		e := p.removal
		if e == nil {
			return
		}
		ui.Column(c).Width(360).Padding(6).Gap(12).Children(func() {
			ui.Text(c, "卸载 "+e.name+"？").FontSize(18).Bold()
			hint := "将从以下配置中移除这个 MCP 连接。"
			if e.projectRoot == "" && e.name == mcp.ExaServer && e.key != "" {
				hint = "将移除此 MCP 连接及保存在以下文件中的 Exa 密钥。"
			}
			ui.Text(c, hint).FontSize(13)
			ui.Text(c, e.path).FontSize(12).TextColor(c.Theme().TextMuted).Selectable()
			if p.problem != "" {
				ui.Text(c, p.problem).TextColor(c.Theme().Danger)
			}
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				ui.Button(c, "取消").OnClick(func() { p.removingMCP = false; p.removal = nil; p.problem = "" })
				ui.PrimaryButton(c, "确认卸载").OnClick(func() { a.saveMCPEditor(p, e, true) })
			})
		})
	})
}

func (a *app) openMCPEditor(p *customizationPage, row projectExtension, restore, remove bool) {
	e, err := readMCPEditor(a.configPath, p.path, row, restore)
	if err != nil {
		p.problem = err.Error()
		return
	}
	p.problem, p.notice = "", ""
	if remove {
		p.removal, p.removingMCP = e, true
	} else {
		p.mcpEditor = e
	}
}

func (a *app) saveMCPEditor(p *customizationPage, e *mcpEditor, remove bool) {
	saved, err := e.save(remove)
	if err != nil {
		p.problem = err.Error()
		return
	}
	if e.projectRoot == "" {
		cfg := a.host.settings.Load().Clone()
		cfg.ApplyUserMCPServers(saved)
		a.host.setConfig(cfg)
		if cfg.ExaAPIKey != saved.ExaAPIKey {
			a.applySavedConfig(saved)
		}
	}
	p.selectedPlugin, p.problem = "", ""
	p.mcpEditor, p.removal, p.removingMCP = nil, nil, false
	p.tab = mcpTab
	a.loadCustomization(p, func() (string, error) {
		action := "已保存 "
		if remove {
			action = "已卸载 "
		}
		if e.projectRoot != "" {
			return action + e.name + "。新会话按项目的信任状态加载服务。", nil
		}
		return action + e.name + "，新会话生效。", nil
	})
}

func (a *app) mcpEditorView(c *ui.Context, p *customizationPage) {
	e := p.mcpEditor
	title := "配置 " + e.name
	if e.adding {
		title = "添加 MCP"
	}
	ui.Text(c, title).FontSize(27).FontWeight(650)
	ui.Text(c, e.path).FontSize(12).TextColor(c.Theme().TextMuted).Selectable()
	if e.adding && !e.preset {
		textField(c, "名称", "如 docs", &e.name)
	}
	field(c, "连接方式", func() { ui.Select(c, &e.transport, []string{"stdio", "http", "sse"}).Label("连接方式").Height(34) })
	if e.transport == "stdio" {
		textField(c, "启动命令", "可执行文件名或路径", &e.command)
		textField(c, "启动参数（JSON 数组）", `["--stdio"]`, &e.args)
	} else {
		textField(c, "服务地址", "https://example.com/mcp", &e.endpoint)
		ui.Checkbox(c, &e.network, "允许连接此远程服务")
	}
	if e.projectRoot == "" && e.hostedExa() {
		field(c, "Exa API 密钥", func() {
			ui.TextInput(c, &e.key).Password().Label("Exa API 密钥输入框").Placeholder("选填").Height(34)
		})
		ui.Text(c, "留空时使用 EXA_API_KEY 环境变量，未设置则使用匿名额度。").FontSize(12).TextColor(c.Theme().TextMuted)
	}
	ui.Button(c, "高级配置").AlignSelf(ui.Start).OnClick(func() { e.advanced = !e.advanced })
	if e.advanced {
		field(c, "其他 MCP 字段（JSON）", func() {
			ui.TextArea(c, &e.extra).Label("高级配置输入框").Placeholder(`{"env":{"NAME":"value"}}`).Lines(3, 8)
		})
	}
	if p.problem != "" {
		ui.Text(c, p.problem).TextColor(c.Theme().Danger).Selectable()
	}
	ui.Row(c).Gap(8).Children(func() {
		ui.PrimaryButton(c, "保存 MCP").Disabled(p.loading).OnClick(func() { a.saveMCPEditor(p, e, false) })
		ui.Button(c, "取消").OnClick(func() { p.mcpEditor = nil; p.problem = "" })
	})
}
