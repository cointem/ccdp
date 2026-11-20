package desktop

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

type sidebarProject struct {
	path, title string
	projectMetadata
	sessions   []savedSession
	lastActive time.Time
	duplicate  bool
}

// Use the complete history for project ordering. Searching narrows
// the displayed rows and temporarily expands groups without changing their
// saved disclosure state.
func (a *app) sidebarProjects(keys map[string]string) []*sidebarProject {
	byPath := map[string]*sidebarProject{}
	var records map[string]projectMetadata
	if a.projects != nil {
		records = a.projects.Items
	}
	groupFor := func(path string) *sidebarProject {
		if group := byPath[path]; group != nil {
			return group
		}
		meta := records[path]
		group := &sidebarProject{path: path, title: cmp.Or(meta.Name, projectName(path)), projectMetadata: meta}
		byPath[path] = group
		return group
	}
	for path := range records {
		groupFor(path)
		keys[path] = path // Persisted keys have been canonicalized already.
	}
	for _, session := range a.allSessionEntries() {
		path, ok := keys[session.Workspace]
		if !ok {
			path = projectKey(session.Workspace)
			keys[session.Workspace] = path
		}
		g := groupFor(path)
		g.sessions = append(g.sessions, session)
		if session.UpdatedAt.After(g.lastActive) {
			g.lastActive = session.UpdatedAt
		}
	}
	nameCount := make(map[string]int)
	for _, g := range byPath {
		nameCount[g.title]++
	}
	query := strings.ToLower(strings.TrimSpace(a.search))
	groups := make([]*sidebarProject, 0, len(byPath))
	for _, g := range byPath {
		g.duplicate = nameCount[g.title] > 1
		if query != "" && !strings.Contains(strings.ToLower(g.title+" "+g.path), query) {
			g.sessions = slices.DeleteFunc(g.sessions, func(s savedSession) bool {
				return !strings.Contains(strings.ToLower(s.Title+" "+s.Workspace), query)
			})
			if len(g.sessions) == 0 {
				continue
			}
		}
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *sidebarProject) int {
		if a.Pinned != b.Pinned {
			if a.Pinned {
				return -1
			}
			return 1
		}
		return cmp.Or(b.lastActive.Compare(a.lastActive), strings.Compare(a.title, b.title), strings.Compare(a.path, b.path))
	})
	return groups
}

func (a *app) projectList(c *ui.Context) {
	list := ui.Scroll(c).Grow(1).MinHeight(0).Padding(8, 12).Gap(6)
	keys := ui.Local(list, "workspace-keys", func() map[string]string { return make(map[string]string) })
	groups := a.sidebarProjects(*keys)
	searching := strings.TrimSpace(a.search) != ""
	list.Children(func() {
		if searching && len(groups) == 0 {
			ui.Text(c, "没有找到匹配的项目或会话").FontSize(12).TextColor(c.Theme().TextMuted).Padding(10, 8)
		}
		for _, group := range groups {
			ui.Column(c.Key(group.path)).Gap(2).Children(func() {
				if group.path != "" {
					a.projectHeading(c, group, searching)
				} else {
					ui.Text(c, "未选择项目").FontSize(12).TextColor(c.Theme().TextMuted).Padding(6, 8)
				}
				if !group.Collapsed || searching {
					for _, entry := range group.sessions {
						a.sidebarChat(c, entry)
					}
				}
			})
		}
	})
}

func (a *app) projectHeading(c *ui.Context, g *sidebarProject, searching bool) {
	t := c.Theme()
	disclosure, glyph := "折叠项目", "down"
	if g.Collapsed && !searching {
		disclosure, glyph = "展开项目", "right"
	}
	canFold := !searching && len(g.sessions) > 0
	action := disclosure
	if !canFold {
		action = "编辑项目"
	}
	change := func(edit func(*projectMetadata)) {
		if err := a.projects.update(g.path, edit); err != nil {
			a.notice = fmt.Sprintf("无法保存项目设置：%v", err)
		}
	}
	fold := func() { change(func(m *projectMetadata) { m.Collapsed = !g.Collapsed }) }
	ui.Row(c).AlignItems(ui.Center).Gap(2).Children(func() {
		label := ui.ButtonBase(c).Grow(1).MinWidth(0).Justify(ui.Start).AlignItems(ui.Center).
			Padding(4, 3).Gap(5).Radius(7).Label(action + " · " + g.title).Tooltip(g.path)
		if label.Hovered() {
			label.Background(t.SurfaceHover)
		}
		label.Children(func() {
			if canFold {
				ui.Icon(c, icons[glyph]).Size(12, 12).TextColor(t.TextMuted)
			} else {
				ui.Box(c).Width(12)
			}
			symbol := "folder"
			if g.Pinned {
				symbol = "pin"
			}
			ui.Icon(c, icons[symbol]).Size(15, 15).TextColor(t.TextMuted)
			ui.Column(c).Grow(1).MinWidth(0).Children(func() {
				ui.Text(c, g.title).SingleLine().FontSize(13).FontWeight(550)
				if g.duplicate {
					ui.Text(c, filepath.Dir(g.path)).SingleLine().TruncateStart().FontSize(10).TextColor(t.TextMuted)
				}
			})
		})
		if canFold {
			label.OnClick(fold)
		} else {
			label.Disabled(a.opening).OnClick(func() { a.openProjectPage(g.path, false) })
		}
		more := iconButton(c, "more", "项目菜单 · "+g.title).Size(26, 28).Disabled(a.opening)
		more.PopoverMenuButton(func(m *ui.Menu) {
			m.MaxWidth(340)
			pathLabel := g.path
			if len([]rune(pathLabel)) > 46 {
				pathLabel = filepath.Join("…", filepath.Base(filepath.Dir(g.path)), filepath.Base(g.path))
			}
			if m.Item("编辑项目").Subtitle(pathLabel).Tooltip(g.path).Chosen() {
				a.openProjectPage(g.path, false)
			}
			if m.Item("当前项目的扩展").Chosen() {
				a.openProjectPage(g.path, true)
			}
			m.Separator()
			pin := "置顶项目"
			if g.Pinned {
				pin = "取消置顶"
			}
			if m.Item(pin).Chosen() {
				change(func(meta *projectMetadata) { meta.Pinned = !g.Pinned })
			}
			if m.Item(disclosure).Disabled(!canFold).Chosen() {
				fold()
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorTopEnd, Self: ui.AnchorTopStart})
		iconButton(c, "new", "在 "+g.title+" 新建对话").Size(26, 28).Disabled(a.opening).OnClick(func() {
			a.openProjectForChat(g.path, nil)
		})
	})
}

func (a *app) sidebarChat(c *ui.Context, entry savedSession) {
	t := c.Theme()
	live := a.conversations[entry.ID]
	backgroundRun := entry.ID != a.current && live != nil && live.connected && live.executing()
	row := ui.ButtonBase(c.Key(entry.ID)).Padding(8, 10, 8, 23).Radius(7).
		Label(entry.Title).Tooltip(entry.Title).Busy(backgroundRun).Disabled(a.opening)
	if a.current == entry.ID {
		row.Background(t.Border)
	} else if row.Hovered() {
		row.Background(t.SurfaceHover)
	}
	row.Children(func() {
		ui.Text(c, entry.Title).SingleLine().Grow(1).MinWidth(0).FontSize(13)
		if backgroundRun {
			ui.Spinner(c).Size(10, 10).Label("会话执行中").Tooltip("会话执行中")
		}
	}).OnClick(func() { a.openSession(entry.Workspace, entry.ID) })
}
