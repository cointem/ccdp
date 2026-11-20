package desktop

import (
	"fmt"
	"path/filepath"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/mcp"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type customizationTab int

const (
	pluginsTab customizationTab = iota
	skillsTab
	mcpTab
)

type customizationPage struct {
	tab                                     customizationTab
	extensions                              *projectExtensions
	path, pluginQuery, skillQuery, mcpQuery string
	mcpEditor, removal                      *mcpEditor
	removingMCP                             bool
	loading, changing                       bool
	scope                                   int // all, personal or project
	generation                              uint64
	installed                               []installedItem
	pluginError, notice, problem            string
	selectedPlugin                          string
	selectedSkill                           projectExtension
	showSkill, confirmRemoval               bool
}

// Only bundles with a local install receipt can use the plugin uninstaller.
type installedItem struct {
	key, name, detail, icon string
	bundle                  *customPlugin
	server                  *projectExtension
}

func installedItems(plugins []customPlugin, servers []projectExtension) []installedItem {
	items := make([]installedItem, 0, len(plugins)+len(servers))
	for i := range plugins {
		plugin := &plugins[i]
		items = append(items, installedItem{
			key: plugin.ID, name: plugin.Name, detail: plugin.Description, icon: "extensions", bundle: plugin,
		})
	}
	for i := range servers {
		server := &servers[i]
		items = append(items, installedItem{
			key: "mcp:" + string(server.source) + ":" + server.name, name: server.name,
			detail: "MCP 服务 · " + server.scopeLabel() + " · " + server.detail, icon: "provider", server: server,
		})
	}
	return items
}

func (a *app) openCustomization(path string) {
	p := &customizationPage{path: projectKey(path)}
	a.pluginsPage = p
	a.loadCustomization(p, nil)
}

// Refresh the lists together so an installed plugin and its skills appear
// in the same update. Directory operations are kept off the window thread.
func (a *app) loadCustomization(p *customizationPage, change func() (string, error)) {
	if p.changing || (p.loading && change == nil) {
		return
	}
	p.generation++
	generation := p.generation
	p.loading, p.changing, p.problem, p.notice = true, change != nil, "", ""
	go func() {
		var notice string
		var changeErr error
		if change != nil {
			notice, changeErr = change()
		}
		plugins, pluginErr := installedCustomPlugins(userSkillDirectory())
		extensions := loadCustomizationExtensions(p.path, a.configPath)
		a.dispatch(func() {
			if a.pluginsPage != p || generation != p.generation {
				return
			}
			p.installed, p.extensions = installedItems(plugins, extensions.servers), extensions
			p.loading, p.changing = false, false
			p.pluginError = ""
			if pluginErr != nil {
				p.pluginError = pluginErr.Error()
			}
			if changeErr != nil {
				p.problem = changeErr.Error()
			} else {
				p.notice = notice
			}
		})
	}()
}

func userSkillDirectory() string {
	return filepath.Join(filepath.Dir(config.ConfigPath()), "skills")
}

func (a *app) importDirectory(p *customizationPage, skill, project bool) {
	title := "选择包含插件清单的目录"
	if skill {
		title = "选择包含 SKILL.md 的技能目录"
	}
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: title, Directory: true})
	if err != nil {
		p.problem = err.Error()
		return
	}
	if len(paths) == 0 {
		return
	}
	source := paths[0]
	a.loadCustomization(p, func() (string, error) {
		if skill {
			return "技能已添加", installLocalSkill(source, p.path, project)
		}
		return "插件已安装", installCustomPlugin(userSkillDirectory(), source)
	})
}

func (a *app) customizationView(c *ui.Context) {
	p, t := a.pluginsPage, c.Theme()
	width, _ := c.Size()
	side := float32(216)
	if width < 900 {
		side = 164
	}
	room := width - navigationRailWidth
	rail := measurePane(&a.sidebarWidth, side, 164, room-420-1)
	mainWidth := room - rail.width - 1
	pad := max(float32(24), (mainWidth-900)/2)
	wide := mainWidth-2*pad >= 620
	sideBG := ui.Hex("#f8f8f7")
	if t.Dark {
		sideBG = ui.Hex("#1c1c1c")
	}
	rail.split(c.Key("customization"), room, false, func() {
		ui.Column(c).Fill().Background(sideBG).Padding(8, 10).Gap(4).Children(func() {
			ui.Text(c, "自定义").FontSize(20).FontWeight(650).Margin(0, 8, 14, 8)
			for i, item := range []struct{ icon, title string }{{"customize", "插件"}, {"skill", "技能"}, {"provider", "MCP"}} {
				tab := customizationTab(i)
				b := quietButton(c, item.icon, item.title)
				if p.tab == tab {
					b.Background(t.Border).Current(ui.CurrentPage)
				}
				b.OnClick(func() { p.tab, p.selectedPlugin, p.notice, p.problem, p.mcpEditor = tab, "", "", "", nil })
			}
		})
	}, func() {
		ui.Column(c).Fill().MinWidth(0).Children(func() {
			ui.Scroll(c.Key(struct {
				Tab     customizationTab
				Plugin  string
				Editing *mcpEditor
			}{p.tab, p.selectedPlugin, p.mcpEditor})).Grow(1).MinHeight(0).Padding(18, pad, 32, pad).Gap(20).Children(func() {
				if p.mcpEditor != nil {
					a.mcpEditorView(c, p)
					return
				}
				for _, item := range p.installed {
					if item.key == p.selectedPlugin {
						a.pluginDetail(c, p, item)
						return
					}
				}
				a.customizationHeader(c, p, wide)
				if p.problem != "" {
					ui.Text(c, p.problem).FontSize(13).TextColor(t.Danger).Selectable()
				} else if p.notice != "" {
					ui.Text(c, p.notice).FontSize(13).TextColor(t.TextMuted)
				}
				if p.tab == skillsTab {
					a.skillList(c, p, wide)
				} else {
					a.installedList(c, p)
				}
			})
		})
	})
	a.mcpRemovalDialog(c, p)
	ui.Modal(c, &p.showSkill, func() {
		skill := p.selectedSkill
		ui.Column(c).Width(380).Padding(6).Gap(12).Children(func() {
			ui.Text(c, skill.name).FontSize(20).Bold()
			if skill.detail != "" {
				ui.Text(c, skill.detail).FontSize(14).Selectable()
			}
			ui.Text(c, skill.sourceLabel()+" · "+skill.path).FontSize(12).TextColor(t.TextMuted).Selectable()
			ui.Button(c, "关闭").AlignSelf(ui.End).OnClick(func() { p.showSkill = false })
		})
	})
	ui.Modal(c, &p.confirmRemoval, func() {
		ui.Column(c).Width(340).Padding(6).Gap(14).Children(func() {
			ui.Text(c, "卸载这个插件及其附带的技能？").FontSize(17).Bold()
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				ui.Button(c, "取消").OnClick(func() { p.confirmRemoval = false })
				ui.PrimaryButton(c, "卸载").OnClick(func() {
					id := p.selectedPlugin
					p.confirmRemoval, p.selectedPlugin = false, ""
					a.loadCustomization(p, func() (string, error) {
						return "插件已卸载", uninstallCustomPlugin(userSkillDirectory(), id)
					})
				})
			})
		})
	})
}

func (a *app) customizationHeader(c *ui.Context, p *customizationPage, wide bool) {
	title, query := "插件", &p.pluginQuery
	if p.tab == skillsTab {
		title, query = "技能", &p.skillQuery
	} else if p.tab == mcpTab {
		title, query = "MCP", &p.mcpQuery
	}
	heading := func() { ui.Text(c, title).FontSize(27).FontWeight(650).Grow(1) }
	actions := func() {
		ui.TextInput(c.Key(title), query).Search().Label("搜索" + title).Placeholder("搜索" + title).MinWidth(80).MaxWidth(208).Grow(1).Height(34)
		iconButton(c, "refresh", "刷新"+title).Disabled(p.loading).OnClick(func() { a.loadCustomization(p, nil) })
		if p.tab == mcpTab {
			ui.PrimaryButton(c, "添加 MCP").Disabled(p.loading).OnClick(func() { a.openMCPEditor(p, projectExtension{}, false, false) })
		} else if p.tab == pluginsTab {
			ui.PrimaryButton(c, "安装本地插件").Disabled(p.changing).OnClick(func() { a.importDirectory(p, false, false) })
		} else if p.path == "" {
			ui.PrimaryButton(c, "添加").Label("添加技能").Disabled(p.changing).OnClick(func() { a.importDirectory(p, true, false) })
		} else {
			ui.PrimaryButton(c, "添加 ▾").Label("添加技能").Disabled(p.changing).PopoverMenuButton(func(menu *ui.Menu) {
				if menu.Item("添加到个人技能").Chosen() {
					a.importDirectory(p, true, false)
				}
				if menu.Item("添加到当前项目").Chosen() {
					a.importDirectory(p, true, true)
				}
			})
		}
	}
	if wide {
		ui.Row(c).AlignItems(ui.Center).Gap(10).Children(func() { heading(); actions() })
	} else {
		heading()
		ui.Row(c).AlignItems(ui.Center).Gap(8).Children(actions)
	}
}

func matchesCustomSearch(query, name, description string) bool {
	return strings.Contains(strings.ToLower(name+" "+description), strings.ToLower(strings.TrimSpace(query)))
}

func customRow(c *ui.Context, glyph, title, detail string, action func()) {
	t := c.Theme()
	button := ui.ButtonBase(c).Label(title).Tooltip(detail).Padding(8).Gap(12).Radius(8).AlignItems(ui.Center).Justify(ui.Start)
	if button.Hovered() {
		button.Background(t.SurfaceHover)
	}
	button.Children(func() {
		ui.Box(c).Size(40, 40).Radius(9).Center().Background(t.Surface).Children(func() {
			ui.Image(c, icons[glyph]).Size(23, 23)
		})
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Text(c, title).SingleLine().FontSize(14).FontWeight(600)
			if detail != "" {
				ui.Text(c, detail).SingleLine().FontSize(13).TextColor(t.TextMuted)
			}
		})
	}).OnClick(action)
}

func (a *app) installedList(c *ui.Context, p *customizationPage) {
	mcpOnly := p.tab == mcpTab
	kind, query := "插件", p.pluginQuery
	if mcpOnly {
		kind, query = "MCP", p.mcpQuery
	}
	if p.extensions == nil {
		ui.Text(c, "正在读取"+kind+"…").TextColor(c.Theme().TextMuted)
		return
	}
	problem := p.extensions.problem
	if !mcpOnly {
		problem = strings.TrimSpace(p.pluginError + "\n" + problem)
	}
	if problem != "" {
		ui.Text(c, problem).TextColor(c.Theme().Danger).Selectable()
	}
	total, shown := 0, 0
	for _, item := range p.installed {
		if mcpOnly && item.server == nil {
			continue
		}
		total++
		if matchesCustomSearch(query, item.name+" "+item.key, item.detail) {
			shown++
			customRow(c.Key(item.key), item.icon, item.name, item.detail, func() { p.selectedPlugin, p.notice, p.problem = item.key, "", "" })
		}
	}
	if shown == 0 && problem == "" {
		label := "没有匹配的" + kind
		if total == 0 {
			label = "还没有安装" + kind
		}
		ui.Text(c, label).TextColor(c.Theme().TextMuted)
	}
	if mcpOnly && p.extensions.restoreExa && matchesCustomSearch(query, "exa", "网页搜索") {
		ui.Button(c, "重新安装 Exa").AlignSelf(ui.Start).Disabled(p.loading).OnClick(func() {
			a.openMCPEditor(p, projectExtension{name: mcp.ExaServer}, true, false)
		})
	}
}

func (a *app) skillList(c *ui.Context, p *customizationPage, wide bool) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		labels := []string{"全部", "个人"}
		if p.path != "" {
			labels = append(labels, "当前项目")
		}
		for i, label := range labels {
			b := ui.Button(c, label).Radius(14).FontSize(12).Border(0, ui.Transparent).Background(ui.Transparent)
			if label == "当前项目" {
				b.Tooltip(a.projectTitle(p.path) + "\n" + p.path)
			}
			if i == p.scope {
				b.Background(c.Theme().Surface).AccessSelected(true)
			}
			b.OnClick(func() { p.scope = i })
		}
	})
	if p.extensions == nil {
		ui.Text(c, "正在读取技能…").TextColor(c.Theme().TextMuted)
		return
	}
	if problem := strings.TrimSpace(p.extensions.problem + "\n" + p.extensions.skillsProblem); problem != "" {
		ui.Text(c, problem).TextColor(c.Theme().Danger).Selectable()
		if len(p.extensions.skills) == 0 {
			return
		}
	}
	var rows []projectExtension
	for _, skill := range p.extensions.skills {
		project := skill.source == config.SourceProject || skill.source == config.SourceProjectLocal
		if (p.scope == 1 && project) || (p.scope == 2 && !project) || !matchesCustomSearch(p.skillQuery, skill.name, skill.detail) {
			continue
		}
		rows = append(rows, skill)
	}
	if len(rows) == 0 {
		text := "没有匹配的技能"
		if p.loading {
			text = "正在读取技能…"
		} else if len(p.extensions.skills) == 0 {
			text = "还没有安装技能。点「添加」选择包含 SKILL.md 的目录。"
		}
		ui.Text(c, text).TextColor(c.Theme().TextMuted)
		return
	}
	columns := 1
	if wide {
		columns = 2
	}
	ui.Column(c).Gap(8).Children(func() {
		for i := 0; i < len(rows); i += columns {
			ui.Row(c).Gap(16).AlignItems(ui.Stretch).Children(func() {
				for j := 0; j < columns; j++ {
					ui.Column(c).Basis(0).Grow(1).MinWidth(0).Children(func() {
						if i+j < len(rows) {
							skill := rows[i+j]
							customRow(c.Key(skill.path), "skillColor", skill.name, skill.detail, func() { p.selectedSkill, p.showSkill = skill, true })
						}
					})
				}
			})
		}
	})
}

func (a *app) pluginDetail(c *ui.Context, p *customizationPage, item installedItem) {
	back := "返回插件列表"
	if p.tab == mcpTab {
		back = "返回 MCP 列表"
	}
	quietButton(c, "back", back).AlignSelf(ui.Start).OnClick(func() { p.selectedPlugin = "" })
	ui.Text(c, item.name).FontSize(27).FontWeight(650)
	if server := item.server; server != nil {
		ui.Text(c, item.detail).FontSize(14).Selectable()
		path := server.path
		if path == "" {
			path = a.configPath
		}
		ui.Text(c, path).FontSize(13).TextColor(c.Theme().TextMuted).Selectable()
		ui.Row(c).Gap(8).Children(func() {
			ui.PrimaryButton(c, "配置 MCP").Disabled(p.loading).OnClick(func() { a.openMCPEditor(p, *server, false, false) })
			ui.Button(c, "卸载 MCP").Disabled(p.loading).OnClick(func() { a.openMCPEditor(p, *server, false, true) })
		})
		if p.problem != "" {
			ui.Text(c, p.problem).TextColor(c.Theme().Danger).Selectable()
		}
		return
	}
	plugin := item.bundle
	if plugin.Description != "" {
		ui.Text(c, plugin.Description).Selectable()
	}
	ui.Text(c, fmt.Sprintf("%s  %s", plugin.ID, plugin.Version)).FontSize(13).TextColor(c.Theme().TextMuted)
	ui.Text(c, "包含的技能").FontSize(16).Bold()
	for _, name := range plugin.Skills {
		ui.Text(c, name).FontSize(14)
	}
	ui.Button(c, "卸载插件").AlignSelf(ui.Start).Disabled(p.loading).OnClick(func() { p.confirmRemoval = true })
}
