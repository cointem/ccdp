package desktop

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
	"ccdp/internal/workspace"

	"github.com/egoist/mygo/ui"
)

type projectTab int

const (
	projectDetailsTab projectTab = iota
	projectInstructionsTab
	projectExtensionsTab
)

type projectPage struct {
	path, name, originalName string
	sessionCount             int
	tab                      projectTab
	instructions             []projectInstruction
	fileIndex                int
	extensions               *projectExtensions
	loading                  bool
	matchingWorkspaces       map[string]bool
	notice, problem          string
	discard, reloadPrompt    bool
}

func (a *app) projectTitle(path string) string {
	if name := a.projects.get(path).Name; name != "" {
		return name
	}
	return projectName(path)
}

func (a *app) openProjectPage(path string, extensions bool) {
	path = projectKey(path)
	if path == "" || a.opening {
		return
	}
	name := a.projects.get(path).Name
	p := &projectPage{path: path, name: name, originalName: name, matchingWorkspaces: make(map[string]bool)}
	a.projectPage = p
	for _, entry := range a.allSessionEntries() {
		if !entry.Draft && p.matchesWorkspace(entry.Workspace) {
			p.sessionCount++
		}
	}
	if extensions {
		a.selectProjectTab(p, projectExtensionsTab)
	}
}

func (p *projectPage) matchesWorkspace(path string) bool {
	if path == p.path {
		return true
	}
	matches, ok := p.matchingWorkspaces[path]
	if !ok {
		matches = projectKey(path) == p.path
		p.matchingWorkspaces[path] = matches
	}
	return matches
}

// Two chats in one project can publish different tools. Prefer the currently
// selected chat, then the most recently active connected root in that project.
func (a *app) projectCatalog(p *projectPage) (*protocol.CatalogSnapshot, string) {
	var recent *conversation
	for id, conv := range a.conversations {
		if conv.client == nil || !conv.connected || conv.rootID != id || !p.matchesWorkspace(conv.view.Settings.Workspace) {
			continue
		}
		if id == a.current {
			return &conv.view.Catalog, conv.title
		}
		if recent == nil || conv.updatedAt.After(recent.updatedAt) {
			recent = conv
		}
	}
	if recent == nil {
		return nil, ""
	}
	return &recent.view.Catalog, recent.title
}

func (a *app) selectProjectTab(p *projectPage, tab projectTab) {
	p.tab, p.notice, p.problem = tab, "", ""
	switch tab {
	case projectInstructionsTab:
		if p.instructions == nil {
			for _, name := range []string{workspace.InstructionFile, workspace.ProjectInstructionFile} {
				p.instructions = append(p.instructions, readProjectInstruction(p.path, name))
			}
		}
	case projectExtensionsTab:
		if p.extensions == nil && !p.loading {
			a.refreshProjectExtensions(p)
		}
	}
}

func (a *app) refreshProjectExtensions(p *projectPage) {
	if p.loading {
		return
	}
	p.extensions, p.loading = nil, true
	go func() {
		extensions := loadProjectExtensions(p.path, a.configPath)
		a.dispatch(func() {
			if a.projectPage == p {
				p.extensions, p.loading = extensions, false
			}
		})
	}()
}

func (p *projectPage) dirty() bool {
	if p.name != p.originalName {
		return true
	}
	for _, file := range p.instructions {
		if file.text != file.original {
			return true
		}
	}
	return false
}

func (a *app) projectView(c *ui.Context) {
	p, t := a.projectPage, c.Theme()
	card, side := ui.Hex("#ffffff"), ui.Hex("#f1f1ee")
	if t.Dark {
		card, side = ui.Hex("#272727"), ui.Hex("#191919")
	}
	width, _ := c.Size()
	width -= navigationRailWidth
	rail := a.settingsSidebar(width)
	pad := max(float32(24), (width-rail.width-1-758)/2)
	rail.split(c.Key("project-page"), width, false, func() {
		ui.Column(c).Fill().Background(side).Children(func() {
			ui.Column(c).Padding(10, 14).Gap(6).Children(func() {
				ui.Text(c, a.projectTitle(p.path)).SingleLine().Tooltip(p.path).FontSize(22).FontWeight(650).Margin(16, 8, 10, 8)
				for tab, item := range []struct{ icon, title string }{{"folder", "项目信息"}, {"code", "项目指令"}, {"provider", "扩展"}} {
					row := quietButton(c, item.icon, item.title)
					if p.tab == projectTab(tab) {
						row.Background(t.Border)
					}
					row.OnClick(func() { a.selectProjectTab(p, projectTab(tab)) })
				}
			})
		})
	}, func() {
		ui.Column(c).Fill().MinWidth(0).Children(func() {
			ui.Scroll(c.Key(p.tab)).Grow(1).MinHeight(0).Padding(20, pad, 28, pad).Gap(20).Children(func() {
				switch p.tab {
				case projectDetailsTab:
					ui.Text(c, "编辑项目").FontSize(28).FontWeight(650)
					settingsCard(c, card, "项目信息", func() {
						textField(c, "显示名称", projectName(p.path), &p.name)
						field(c, "工作目录", func() { ui.Text(c, p.path).Selectable().FontSize(13) })
						field(c, "会话数", func() { ui.Text(c, fmt.Sprintf("%d 个会话", p.sessionCount)).FontSize(13) })
					})
				case projectInstructionsTab:
					a.projectInstructionsView(c, p, card)
				case projectExtensionsTab:
					catalog, title := a.projectCatalog(p)
					projectExtensionsView(c, card, p.extensions, catalog, title, func() { a.refreshProjectExtensions(p) })
				}
			})
			if p.tab != projectExtensionsTab {
				a.projectFooter(c, p, pad)
			}
		})
	})
	ui.Modal(c, &p.discard, func() {
		ui.Column(c).Width(330).Padding(6).Gap(16).Children(func() {
			ui.Text(c, "放弃未保存的项目更改？").FontSize(18).Bold()
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				ui.Button(c, "放弃更改").OnClick(func() { a.showDestination(a.afterDiscard) })
				ui.PrimaryButton(c, "继续编辑").OnClick(func() { p.discard = false })
			})
		})
	})
	ui.Modal(c, &p.reloadPrompt, func() {
		ui.Column(c).Width(330).Padding(6).Gap(16).Children(func() {
			ui.Text(c, "用磁盘内容替换这份指令草稿？").FontSize(17).Bold()
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				ui.Button(c, "取消").OnClick(func() { p.reloadPrompt = false })
				ui.PrimaryButton(c, "读取磁盘内容").OnClick(func() { p.reloadPrompt = false; p.reloadInstruction() })
			})
		})
	})
}

func (p *projectPage) reloadInstruction() {
	doc := &p.instructions[p.fileIndex]
	fresh := readProjectInstruction(p.path, doc.name)
	if fresh.readError != nil {
		p.problem = fresh.readError.Error()
		return
	}
	*doc = fresh
	p.notice, p.problem = "已重新读取", ""
}

func (a *app) projectInstructionsView(c *ui.Context, p *projectPage, card ui.Color) {
	ui.Text(c, "项目指令").FontSize(28).FontWeight(650)
	settingsCard(c, card, "指令文件", func() {
		ui.Segmented(c, &p.fileIndex, workspace.InstructionFile, workspace.ProjectInstructionFile).Label("选择指令文件").AlignSelf(ui.Start).OnChange(func() {
			p.notice, p.problem = "", ""
		})
		doc := &p.instructions[p.fileIndex]
		if doc.readError != nil {
			ui.Text(c, "无法读取 "+doc.name).FontWeight(600)
			ui.Text(c, doc.readError.Error()).TextColor(c.Theme().Danger).Selectable()
		} else {
			ui.TextArea(c.Key(doc.name), &doc.text).Label("项目指令输入框").Lines(9, 16).FontSize(14).
				Placeholder("填写适用于此项目的构建命令、测试方式或代码约定…")
		}
		ui.Button(c, "重新读取").AlignSelf(ui.Start).OnClick(func() {
			if doc.text != doc.original {
				p.reloadPrompt = true
			} else {
				p.reloadInstruction()
			}
		})
	})
}

func (a *app) projectFooter(c *ui.Context, p *projectPage, pad float32) {
	label, dirty := "保存名称", p.name != p.originalName
	if p.tab == projectInstructionsTab {
		doc := &p.instructions[p.fileIndex]
		label, dirty = "保存指令", doc.readError == nil && doc.text != doc.original
	}
	save := func() {
		if !dirty {
			return
		}
		var err error
		if p.tab == projectDetailsTab {
			name := strings.TrimSpace(p.name)
			var conflict bool
			err = a.projects.update(p.path, func(meta *projectMetadata) {
				conflict = meta.Name != p.originalName && meta.Name != name
				if !conflict {
					meta.Name = name
				}
			})
			if err == nil && conflict {
				p.originalName = a.projects.get(p.path).Name
				p.problem = "显示名称已在其他窗口改为「" + a.projectTitle(p.path) + "」。再次保存可使用当前输入。"
				return
			}
			if err == nil {
				p.name, p.originalName = name, name
			}
		} else {
			err = p.instructions[p.fileIndex].save(a.ctx, p.path)
		}
		if err != nil {
			p.problem = err.Error()
			return
		}
		p.notice, p.problem = "已保存", ""
	}
	ui.Row(c).Padding(12, pad).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border).AlignItems(ui.Center).Gap(12).Children(func() {
		hint, color := p.notice, c.Theme().TextMuted
		if p.problem != "" {
			hint, color = p.problem, c.Theme().Danger
		} else if dirty {
			hint = "有未保存的更改"
		}
		ui.Text(c, hint).FontSize(12).TextColor(color).Grow(1).MinWidth(0)
		ui.PrimaryButton(c, label).Padding(8, 16).Disabled(!dirty).OnClick(save)
		c.OnWindowShortcut(ui.Cmd, ui.KeyS, save)
	})
}
