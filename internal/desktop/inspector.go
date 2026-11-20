package desktop

import (
	"encoding/json"
	"fmt"
	"github.com/egoist/mygo"
	"hash/fnv"
	"slices"
	"strings"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

// A tab belongs to an opened object. The source can be a child conversation
// with its own workspace and tool-call identities.
type inspectorPage struct {
	line              int
	revealLine        bool
	key, title, glyph string
	source            *conversation
	agent             *agentDetail
	file              *inspectorFile
	activity          *protocol.TranscriptItem
}

type inspectorState struct {
	open    bool
	current *inspectorPage
	reveal  *inspectorPage
	pages   []*inspectorPage
}

func (s *inspectorState) selectPage(key, title, glyph string, source *conversation) *inspectorPage {
	for _, p := range s.pages {
		if p.key == key && p.source == source {
			s.current, s.reveal, s.open = p, p, true
			return p
		}
	}
	p := &inspectorPage{key: key, title: title, glyph: glyph, source: source}
	s.pages = append(s.pages, p)
	s.current, s.reveal, s.open = p, p, true
	return p
}

func (s *inspectorState) closePage(page *inspectorPage) {
	i := slices.Index(s.pages, page)
	if i < 0 {
		return
	}
	s.pages = slices.Delete(s.pages, i, i+1)
	if len(s.pages) == 0 {
		s.current, s.open = nil, false
	} else if s.current == page {
		s.current = s.pages[min(i, len(s.pages)-1)]
		s.reveal = s.current
	}
}

func (a *app) inspectorFor(source *conversation) *inspectorState {
	if root := a.conversations[source.rootID]; root != nil {
		return &root.inspector
	}
	return &source.inspector
}

// The open report belongs to the clicked event, even if the same agent later
// starts another task. The directory supplies the current run for live views.
type agentDetail struct {
	id                                  protocol.SessionID
	name, task, status, report, eventID string
	isReport, loading                   bool
	problem                             string
}

func (a *app) inspectorView(c *ui.Context, root *conversation, width float32) {
	page, t := root.inspector.current, c.Theme()
	ui.Column(c.Key(page)).Role(ui.RoleGroup).Label("右栏").Fill().MinHeight(0).Background(t.Background).Children(func() {
		if page == nil {
			return
		}
		switch {
		case page.agent != nil:
			a.agentPanel(c, root, page, width)
		case page.activity != nil:
			a.activityPanel(c, page)
		case page.file != nil:
			a.filePanel(c, page)
		}
	})
}

func (a *app) paneToolbar(c *ui.Context, root *conversation, width float32) {
	s := &root.inspector
	ui.Row(c).Width(width).MinWidth(0).Gap(3).AlignItems(ui.Center).Children(func() {
		ui.ScrollHorizontal(c).Grow(1).MinWidth(0).Height(32).AlignItems(ui.Center).Gap(3).Children(func() {
			for _, page := range s.pages {
				active := page == s.current
				tab := ui.Row(c.Key(page)).AlignItems(ui.Center).Radius(7).Shrink(0)
				if page == s.reveal {
					tab.ScrollIntoView()
					s.reveal = nil
				}
				if active {
					tab.Background(c.Theme().Background)
				}
				tab.Children(func() {
					b := quietButton(c, page.glyph, page.title).Label("切换到 "+page.title).Tooltip(page.title).MaxWidth(148).Padding(5, 7)
					if active {
						b.Current(ui.CurrentPage)
					}
					b.OnClick(func() { s.current = page })
					if active {
						iconButton(c, "close", "关闭 "+page.title).Size(22, 24).OnClick(func() { s.closePage(page) })
					}
				})
			}
		})
		a.paneMenu(c, root, "new", "打开内容")
		iconButton(c, "panel-right", "收起右栏").OnClick(func() { s.open = false })
	})
}

func (a *app) paneMenu(c *ui.Context, root *conversation, glyph, label string) {
	iconButton(c, glyph, label).PopoverMenuButton(func(m *ui.Menu) {
		if m.Item("打开项目文件…").Disabled(root.view.Settings.Workspace == "").Chosen() {
			paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "打开项目文件", DefaultPath: root.view.Settings.Workspace})
			if err != nil {
				root.notice = err.Error()
			} else if len(paths) > 0 {
				a.inspectFile(root, paths[0])
			}
		}
		if len(root.children) > 0 {
			m.Separator()
		}
		for _, ch := range root.children {
			d := childDetail(&ch)
			if agentMenuItem(m, d).Chosen() {
				a.inspectAgent(root, d)
			}
		}
	})
}

func childStatus(s string) string {
	switch s {
	case "queued", "starting", "accepted":
		return "已派发"
	case "running":
		return "运行中"
	case "waiting_approval":
		return "待批准"
	case "waiting_question":
		return "待回复"
	case "settling", "finishing":
		return "收尾中"
	case "stopping":
		return "停止中"
	case "succeeded", "completed", "success":
		return "已完成"
	case "partial":
		return "部分完成"
	case "failed", "error":
		return "失败"
	case "cancelled", "interrupted", "stopped":
		return "已停止"
	case "blocked":
		return "受阻"
	case "limit_reached", "resource_limit":
		return "已达上限"
	default:
		return "状态未知"
	}
}

// Keep each choice on one line. Normal completion needs no badge; the full
// state is still available on hover.
func agentMenuItem(m *ui.Menu, d agentDetail) *ui.MenuItem {
	item := m.Item(d.name).ID(d.id.String() + "/" + d.eventID).Tooltip(d.status)
	switch d.status {
	case "已完成", "":
	case "运行中", "进行中":
		item.Trailing("●")
	default:
		item.Trailing(d.status)
	}
	return item
}

func childFor(root *conversation, id protocol.SessionID, item *protocol.TranscriptItem) *protocol.ChildSession {
	for i := range root.children {
		ch := &root.children[i]
		if id != "" && ch.SessionID == id {
			return ch
		}
		if id == "" && item != nil && item.CallID != "" && ch.ParentCallID == item.CallID &&
			(ch.ParentSessionID == "" || ch.ParentSessionID == root.view.SessionID) &&
			(ch.ParentTurnID == "" || item.TurnID == "" || sameTurn(ch.ParentTurnID, item.TurnID)) {
			return ch
		}
	}
	return nil
}

func childDetail(ch *protocol.ChildSession) agentDetail {
	name := strings.TrimSpace(ch.Name)
	if name == "" {
		name = shorten(strings.Join(strings.Fields(ch.Title), " "), 32)
	}
	if name == "" {
		name = "子智能体"
	}
	return agentDetail{id: ch.SessionID, name: name, task: ch.Title, status: childStatus(ch.Run.Status)}
}

func spawnDetail(root *conversation, item protocol.TranscriptItem) agentDetail {
	var args struct{ Name, Task string }
	var receipt protocol.AgentReceipt
	_ = json.Unmarshal(item.Args, &args)
	_ = json.Unmarshal([]byte(item.Text), &receipt)
	if ch := childFor(root, receipt.AgentID, &item); ch != nil {
		return childDetail(ch)
	}
	d := agentDetail{eventID: item.ID, id: receipt.AgentID, name: args.Name, task: args.Task, status: toolState(item)}
	if d.name == "" {
		d.name = shorten(strings.Join(strings.Fields(args.Task), " "), 32)
	}
	if d.name == "" {
		d.name = "子智能体"
	}
	if receipt.Outcome != "" {
		d.status = childStatus(receipt.Outcome)
	} else if d.status == "成功" {
		d.status = "已派发"
	} else if d.status != "进行中" && d.status != "等待执行" {
		d.problem = item.Text
	}
	return d
}

func agentMark(c *ui.Context, name string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	colors := [...]string{"#9773db", "#588fcf", "#d679a5", "#56a297", "#c89262", "#7c81cd"}
	ui.Icon(c, icons["agent"]).Size(15, 15).Shrink(0).TextColor(ui.Hex(colors[h.Sum32()%uint32(len(colors))]))
}

func (a *app) agentGroup(c *ui.Context, root *conversation, items []protocol.TranscriptItem) {
	details := make([]agentDetail, 0, len(items))
	states := make(map[string]int)
	var order []string
	for _, item := range items {
		d := spawnDetail(root, item)
		details = append(details, d)
		if d.status == "已完成" {
			continue
		}
		if states[d.status] == 0 {
			order = append(order, d.status)
		}
		states[d.status]++
	}
	label := details[0].name
	var counts []string
	if len(details) > 1 {
		label = fmt.Sprintf("%d 个子智能体", len(details))
		for _, s := range order {
			counts = append(counts, fmt.Sprintf("%d %s", states[s], s))
		}
	} else if details[0].status != "已完成" {
		counts = append(counts, details[0].status)
	}
	b := ui.ButtonBase(c).Label(label).Cursor(ui.CursorPointer).AlignSelf(ui.Start).MaxWidthPercent(100).Padding(5, 3).Gap(5).AlignItems(ui.Center)
	ink := c.Theme().TextMuted
	if b.Hovered() || b.FocusVisible() {
		ink = c.Theme().Text
	}
	b.Children(func() {
		for _, d := range details[:min(len(details), 4)] {
			agentMark(c, d.name)
		}
		ui.Text(c, label).SingleLine().Shrink(1).MinWidth(0).FontSize(13).TextColor(ink)
		if len(counts) > 0 {
			ui.Text(c, strings.Join(counts, " · ")).SingleLine().Shrink(1).MinWidth(0).FontSize(12).TextColor(c.Theme().TextMuted)
		}
	})
	if len(details) == 1 {
		b.OnClick(func() { a.inspectAgent(root, details[0]) })
	} else {
		b.PopoverMenuButton(func(m *ui.Menu) {
			for _, d := range details {
				if agentMenuItem(m, d).Chosen() {
					a.inspectAgent(root, d)
				}
			}
		})
	}
}

func (a *app) collaborationRow(c *ui.Context, root *conversation, item protocol.TranscriptItem) {
	msg, ok := protocol.DecodeCollaboration(item.Text)
	if !ok || msg.Kind != "final_result" {
		return
	}
	d := agentDetail{eventID: item.ID, id: msg.AgentID, name: msg.Name, report: msg.Text, isReport: true}
	if ch := childFor(root, d.id, nil); ch != nil && d.name == "" {
		d.name = childDetail(ch).name
	}
	if d.name == "" {
		d.name = "子智能体"
	}
	d.status = childStatus(msg.Outcome)
	label := d.name + " 的报告"
	if d.status != "已完成" {
		label = d.name + " · " + d.status
	}
	b := quietButton(c.Key(item.ID), "", label).Padding(4, 2).AlignSelf(ui.Start).Cursor(ui.CursorPointer)
	b.OnClick(func() { a.inspectAgent(root, d) })
}

func (a *app) inspectAgent(root *conversation, d agentDetail) {
	if parent := a.conversations[root.rootID]; parent != nil {
		// Both directions of an exchange belong to the same child tab.
		if parent != root && (d.id == "parent" || d.id == parent.view.SessionID) {
			d.id, d.name = root.view.SessionID, root.title
			if ch := childFor(parent, d.id, nil); ch != nil {
				d.name = childDetail(ch).name
			}
		}
		root = parent
	}
	if d.id == root.view.SessionID {
		d.id = ""
		if d.name == "" {
			d.name = "主会话"
		}
	}
	if d.name == "" {
		if info := childFor(root, d.id, nil); info != nil {
			child := childDetail(info)
			d.name, d.task = child.name, child.task
			if !d.isReport {
				d.status = child.status
			}
		} else {
			d.name = "子智能体"
		}
	}
	key := "agent:" + d.id.String()
	if d.id == "" {
		key = "event:" + d.eventID
	}
	page := root.inspector.selectPage(key, d.name, "agent", root)
	loading := page.agent != nil && page.agent.loading
	d.loading = loading
	page.agent = &d
	if loading || d.id == "" || root.directory == nil || a.conversations[d.id.String()] != nil {
		return
	}
	d.loading = true
	go func() {
		client, err := root.directory.OpenReader(a.ctx, d.id)
		var snapshot protocol.SessionView
		if err == nil {
			snapshot, err = client.Snapshot(a.ctx)
		}
		a.dispatch(func() {
			if !slices.Contains(root.inspector.pages, page) {
				return
			}
			page.agent.loading = false
			if err != nil {
				page.agent.problem = "无法打开子会话：" + err.Error()
				return
			}
			if a.conversations[d.id.String()] == nil {
				a.attach(client, root.directory, snapshot, root.rootID)
			}
		})
	}()
}

func (a *app) agentPanel(c *ui.Context, root *conversation, page *inspectorPage, width float32) {
	t := c.Theme()
	d := page.agent
	child := a.conversations[d.id.String()]
	info := childFor(root, d.id, nil)
	name := d.name
	caption := d.status
	if !d.isReport && info != nil {
		caption = childStatus(info.Run.Status)
		name = childDetail(info).name
	}
	ui.Row(c).AlignItems(ui.Center).Padding(6, 10).Gap(6).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		agentMark(c, name)
		ui.Text(c, name).SingleLine().Grow(1).MinWidth(0).FontSize(14).FontWeight(600)
		if child != nil && child.view.Settings.Model.Model != "" {
			ui.Text(c, child.view.Settings.Model.Model).MaxWidth(width * .38).SingleLine().FontSize(11).TextColor(t.TextMuted)
		} else if caption != "" && caption != "已完成" {
			ui.Text(c, caption).FontSize(12).TextColor(t.TextMuted)
		}
	})
	if !d.isReport && child != nil {
		if len(child.view.Transcript) > 0 || len(child.history) > 0 {
			a.transcript(c.Key(child), child, width, false)
		} else if info != nil && info.Run.Output != "" {
			ui.Scroll(c).Grow(1).MinHeight(0).Padding(16).Children(func() { a.chatMarkdown(c, child, info.Run.Output, false) })
		} else {
			ui.Column(c).Grow(1).Padding(16).Children(func() { ui.Text(c, d.task).Selectable() })
		}
	} else {
		ui.Scroll(c.Key(d)).Grow(1).MinHeight(0).Padding(16).Gap(12).Children(func() {
			if d.loading {
				ui.Spinner(c).Size(16, 16).Label("读取子会话")
			}
			if d.problem != "" {
				ui.Text(c, d.problem).FontSize(13).Selectable()
			}
			if d.report != "" {
				source := root
				if child != nil {
					source = child
				}
				a.chatMarkdown(c, source, d.report, false)
			} else if d.task != "" {
				ui.Text(c, d.task).Selectable()
			}
			if child == nil && d.id != "" && !d.loading && root.directory != nil && d.problem != "" {
				ui.Button(c, "重新读取").OnClick(func() {
					next := *d
					next.problem = ""
					a.inspectAgent(root, next)
				})
			}
		})
	}
	if child != nil {
		a.decisions(c, child)
		if child.notice != "" {
			a.banner(c, child.notice, func() { child.notice = "" })
		}
		if a.canMessageChild(child) {
			ui.Column(c.Key("child-composer")).Padding(10, 12).Children(func() {
				ui.Column(c).Padding(8).Radius(14).Border(1, t.Border).Children(func() {
					a.messageField(c, child, "子会话消息", "发给 "+name)
					label := "发送给 " + name
					if child.pending != nil && !child.sending {
						label = "重试发送给 " + name
					}
					ui.PrimaryButton(c, "").Label(label).Tooltip(label).AlignSelf(ui.End).Size(28, 28).Padding(5).Radius(14).
						Disabled(child.sending || a.opening || strings.TrimSpace(child.draft) == "").
						Children(func() { ui.Icon(c, icons["send"]).Size(18, 18) }).OnClick(func() { a.send(child) })
				})
			})
		}
	}
}

func (a *app) agentOperation(c *ui.Context, conv *conversation, item protocol.TranscriptItem) bool {
	tool := strings.ToLower(item.Tool)
	var verb string
	switch tool {
	case "readagent":
		verb = "查看子智能体结果"
	case "sendmessage":
		ui.Text(c, "发送协作消息 · "+toolState(item)).FontSize(13).TextColor(c.Theme().TextMuted)
		return true
	case "followupagent":
		verb = "继续子任务"
	case "stopagent":
		verb = "停止子任务"
	case "waitagent":
		verb = "等待子智能体"
	case "listagents":
		verb = "查看子智能体"
	default:
		return false
	}
	var args struct {
		ID   protocol.SessionID `json:"agent_id"`
		Task string
	}
	_ = json.Unmarshal(item.Args, &args)
	d := agentDetail{id: args.ID, task: args.Task, eventID: item.ID}
	if ch := childFor(conv, args.ID, nil); ch != nil {
		d = childDetail(ch)
	}
	label := verb
	if d.name != "" {
		label += " · " + d.name
	} else {
		d.name = "子智能体"
	}
	failed := toolState(item)
	switch failed {
	case "成功", "进行中", "等待执行":
	default:
		label += " · " + failed
		d.problem = item.Text
	}
	if tool == "readagent" {
		var page protocol.AgentReadPage
		if json.Unmarshal([]byte(item.Text), &page) == nil && page.Text != "" {
			d.report, d.isReport = page.Text, true
		}
	}
	d.eventID = item.ID
	if d.id == "parent" && conv.rootID == conv.view.SessionID.String() {
		d.id = ""
	}
	b := quietButton(c.Key(item.ID), "agent", label).Padding(4, 2).AlignSelf(ui.Start)
	if d.id != "" || d.problem != "" || d.isReport {
		b.OnClick(func() { a.inspectAgent(conv, d) })
	} else {
		b.Disabled(len(conv.children) == 0).PopoverMenuButton(func(m *ui.Menu) {
			for _, child := range conv.children {
				info := childDetail(&child)
				if agentMenuItem(m, info).Chosen() {
					a.inspectAgent(conv, info)
				}
			}
		})
	}
	return true
}
