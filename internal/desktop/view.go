package desktop

import (
	"fmt"
	"slices"
	"strings"

	"ccdp/internal/protocol"

	"github.com/egoist/mygo/ui"
)

var icons = map[string]*ui.SVG{
	"close":       icon(`<path d="m6 6 12 12M18 6 6 18"/>`),
	"agent":       icon(`<path d="m12 2 5 5-5 5-5-5zm-6 7 5 5-5 5-5-5zm12 0 5 5-5 5-5-5zm-6 7 4 4-4 4-4-4z" fill="currentColor" stroke="none"/>`),
	"panel-right": icon(`<rect x="2" y="4" width="20" height="16" rx="3"/><path d="M15 4v16"/>`),
	"book":        icon(`<path d="M12 6c-2-2-5-3-9-2v14c4-1 7 0 9 2 2-2 5-3 9-2V4c-4-1-7 0-9 2zm0 0v14"/>`),
	"terminal":    icon(`<rect x="2.5" y="4" width="19" height="16" rx="3"/><path d="m6 8 4 4-4 4m6-1h4"/>`),
	"edit":        icon(`<path d="m14 5 5 5M4 20l1.5-6L16 3.5a2 2 0 0 1 3 0l1.5 1.5a2 2 0 0 1 0 3L10 18.5z"/>`),
	"thinking":    icon(`<path d="M6 17a7 7 0 1 1 11.7-1.5M9 17v2h6v-2m-5 5h4"/>`),
	"search":      icon(`<circle cx="10.5" cy="10.5" r="6.8"/><path d="m16 16 5 5"/>`),
	"home":        icon(`<path d="m3.5 10 8.5-7 8.5 7v10H15v-6H9v6H3.5z"/>`),
	"customize":   icon(`<circle cx="12" cy="12" r="9.4"/><path d="m8 10 6.5 6.5m-5-5 3.75-3.75 3 3-3.75 3.75m.25-6.25L11 6.5m5.5 5.5 1.5-1.5M8 14l-2.5 2.5"/>`),
	"skill":       icon(`<path d="m12 2 8.5 5v10L12 22l-8.5-5V7zM3.8 7.2 12 12l8.2-4.8M12 12v9.4"/>`),
	"skillColor":  icon(`<path d="m12 2 8.5 5-8.5 5-8.5-5z" fill="#f4b442" stroke="none"/><path d="M3.5 7 12 12v10l-8.5-5z" fill="#fa795a" stroke="none"/><path d="m12 12 8.5-5v10L12 22z" fill="#8973dc" stroke="none"/>`),
	"refresh":     icon(`<path d="M20.3 9A8.5 8.5 0 0 0 5.5 6.6L3 10m0-5v5h5M3.7 15a8.5 8.5 0 0 0 14.8 2.4L21 14m0 5v-5h-5"/>`),
	"new":         icon(`<path d="M12 5v14M5 12h14"/>`),
	"folder":      icon(`<path d="M3 7a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v10H3Z"/>`),
	"chat":        icon(`<path d="M21 11a8 8 0 0 1-8 8H7l-4 3V5a2 2 0 0 1 2-2h8a8 8 0 0 1 8 8Z"/><path d="M7 8h9M7 12h6"/>`),
	"sidebar":     icon(`<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M9 4v16"/>`),
	"send":        icon(`<path d="M12 19V5m-6 6 6-6 6 6"/>`),
	"stop":        icon(`<rect x="6" y="6" width="12" height="12" rx="2"/>`),
	"code":        icon(`<path d="m8 6-6 6 6 6m8-12 6 6-6 6M14 3l-4 18"/>`),
	"more":        icon(`<circle cx="5" cy="12" r="1" fill="currentColor"/><circle cx="12" cy="12" r="1" fill="currentColor"/><circle cx="19" cy="12" r="1" fill="currentColor"/>`),
	"pin":         icon(`<path d="m8 4 9 0-2 6 3 4H6l4-4-2-6ZM12 14v6"/>`),
	"down":        icon(`<path d="m6 9 6 6 6-6"/>`),
	"right":       icon(`<path d="m9 6 6 6-6 6"/>`),
	"back":        icon(`<path d="m13 5-7 7 7 7M6 12h14"/>`),
	"provider":    icon(`<rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.1M7 17.5h.1"/>`),
	"extensions":  icon(`<rect x="3.5" y="3.5" width="7" height="7" rx="1.5"/><rect x="13.5" y="3.5" width="7" height="7" rx="1.5"/><rect x="3.5" y="13.5" width="7" height="7" rx="1.5"/><path d="M17 14v6m-3-3h6"/>`),
	"check":       icon(`<path d="m5 12 4 4L19 6"/>`),
	"auto":        icon(`<path d="m12 3 8 3.5V12c0 4-3.5 7-8 9-4.5-2-8-5-8-9V6.5z"/><path d="m8 10 3 2.5-3 2.5m5 0h3"/>`),
	"manual":      icon(`<path d="M7 12V7a1.3 1.3 0 0 1 2.6 0v4-6.5a1.3 1.3 0 0 1 2.6 0V11 4.5a1.3 1.3 0 0 1 2.6 0V11 6.5a1.3 1.3 0 0 1 2.6 0v6L19 10a1.4 1.4 0 0 1 2.3 1.4l-2.5 6c-.9 2-2.6 3.6-5.8 3.6-2.8 0-4.1-1-5.5-3l-3.3-5A1.4 1.4 0 0 1 6.5 11.5L8 14"/>`),
	"cache":       icon(`<ellipse cx="12" cy="5.5" rx="8" ry="3"/><path d="M4 5.5v12c0 1.7 3.6 3 8 3s8-1.3 8-3v-12M4 11.5c0 1.7 3.6 3 8 3s8-1.3 8-3"/>`),
	"settings":    icon(`<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="3"/><circle cx="15" cy="17" r="3"/>`),
}

func icon(paths string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">` + paths + `</svg>`))
}

func quietButton(c *ui.Context, glyph, label string) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Padding(8, 10).Gap(9).Radius(8).Justify(ui.Start).TextColor(t.Text)
	if b.Hovered() {
		b.Background(t.SurfaceHover)
	}
	return b.Children(func() {
		if glyph != "" {
			ui.Icon(c, icons[glyph]).Size(17, 17)
		}
		if label != "" {
			ui.Text(c, label).SingleLine().FontSize(13)
		}
	})
}

func iconButton(c *ui.Context, glyph, label string) ui.Element {
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(32, 32).Center().Radius(8)
	if b.Hovered() {
		b.Background(c.Theme().SurfaceHover)
	}
	return b.Children(func() { ui.Icon(c, icons[glyph]).Size(17, 17) })
}

func (a *app) view(c *ui.Context) {
	base := *c.Theme()
	if base.Dark {
		base.Background, base.Surface = ui.Hex("#202020"), ui.Hex("#292929")
		base.Border, base.TextMuted = ui.Hex("#3b3b3b"), ui.Hex("#a5a5a5")
		base.Accent, base.AccentText = ui.Hex("#e9e9e9"), ui.Hex("#202020")
	} else {
		base.Background, base.Surface = ui.Hex("#fcfcfb"), ui.Hex("#f2f2ef")
		base.Border, base.TextMuted = ui.Hex("#e3e3df"), ui.Hex("#7b7b75")
		base.Text, base.Accent, base.AccentText = ui.Hex("#282826"), ui.Hex("#30302e"), ui.Hex("#ffffff")
	}
	base.AccentHover, base.AccentPressed = base.Accent, base.Accent
	base.Radius = 8
	c.SetTheme(&base)
	if c.Shortcut(ui.Cmd, ui.KeyComma) {
		a.navigate(settingsDestination)
	}
	ui.Column(c).Fill().Background(base.Background).Children(func() {
		a.windowToolbar(c)
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			a.navigationRail(c)
			ui.Column(c).Grow(1).MinWidth(0).Children(func() {
				switch {
				case a.pluginsPage != nil:
					a.customizationView(c)
				case a.settingsPage != nil:
					a.settingsView(c)
				case a.projectPage != nil:
					a.projectView(c)
				default:
					a.chatView(c)
				}
			})
		})
	})
}

func (a *app) chatView(c *ui.Context) {
	base := c.Theme()
	if a.active() == nil && !a.opening {
		a.newConversation()
	}
	conv := a.active()
	width, _ := c.Size()
	layout := a.chatLayout(width, conv)
	chat := func() {
		ui.Column(c.Key("main-chat")).Fill().MinWidth(0).Children(func() {
			if a.notice != "" {
				a.banner(c, a.notice, func() { a.notice = "" })
			}
			if conv == nil {
				ui.Column(c).Grow(1).Center().Children(func() { ui.Text(c, "正在打开会话…").TextColor(base.TextMuted) })
				return
			}
			if conv.notice != "" {
				a.banner(c, conv.notice, func() { conv.notice = "" })
			}
			if conv.configError != "" {
				ui.Row(c).AlignItems(ui.Center).Gap(8).Padding(6, 12).Children(func() {
					ui.Text(c, "此会话未能应用配置："+conv.configError).Grow(1).FontSize(12).Selectable()
					ui.Button(c, "重试应用").OnClick(func() { a.refreshUserPreferences(conv) })
				})
			}
			ui.Column(c.Key(a.current)).Grow(1).MinHeight(0).Children(func() {
				a.transcript(c, conv, layout.main, layout.left.width == 0)
				a.composer(c, conv)
			})
		})
	}
	withInspector := func() {
		if layout.right.width > 0 {
			layout.right.split(c.Key("inspector-split"), layout.main+1+layout.right.width, true, chat,
				func() { a.inspectorView(c, conv, layout.right.width) })
		} else {
			chat()
		}
	}
	if layout.left.width > 0 {
		layout.left.split(c.Key("sidebar-split"), layout.total, false, func() { a.sidebarView(c) }, withInspector)
	} else {
		withInspector()
	}
}

func (a *app) sidebarView(c *ui.Context) {
	t := c.Theme()
	bg := ui.Hex("#f0f0ed")
	if t.Dark {
		bg = ui.Hex("#191919")
	}
	ui.Column(c).Fill().Background(bg).Children(func() {
		ui.Column(c).Padding(8, 12, 0, 12).Gap(4).Children(func() {
			quietButton(c, "new", "新会话").Disabled(a.opening).OnClick(a.newConversation)
			quietButton(c, "folder", "打开项目").Disabled(a.opening).OnClick(a.chooseWorkspace)
			ui.TextInput(c, &a.search).Label("搜索项目或会话").Placeholder("搜索项目或会话…").Margin(12, 0, 8, 0).FontSize(12).Background(bg).Border(1, t.Border)
		})
		a.projectList(c)
	})
}

func (a *app) windowToolbar(c *ui.Context) {
	t := c.Theme()
	bar := c.TitleBar()
	left := bar.Left + 10
	ui.Row(c.Key("window-toolbar")).Role(ui.RoleToolbar).Label("窗口工具栏").Height(max(44, bar.Height)).Shrink(0).
		Padding(0, bar.Right+14, 0, left).Gap(12).AlignItems(ui.Center).
		Background(windowChromeColor(t)).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).DragWindow().Children(func() {
		if !a.showingChat() {
			iconButton(c, "back", "返回对话").OnClick(func() { a.navigate(chatsDestination) })
			return
		}
		conv := a.active()
		width, _ := c.Size()
		layout := a.chatLayout(width, conv)
		label := "显示侧栏"
		if layout.left.width > 0 {
			label = "收起侧栏"
		}
		toggle := iconButton(c, "sidebar", label).OnClick(func() {
			if layout.compact {
				conv.inspector.open, a.sidebar = false, true
			} else {
				a.sidebar = !a.sidebar
			}
		})
		if layout.left.width > 0 {
			// Align the conversation title with the pane below the toolbar.
			toggle.Margin(0, max(0, navigationRailWidth+layout.left.width+1-left-32), 0, 0)
		}
		title := "新会话"
		if conv != nil {
			title = conv.title
		}
		ui.Text(c, title).SingleLine().FontSize(14).FontWeight(600).Grow(1).MinWidth(0)
		if conv != nil && conv.rootID != "" && conv.rootID != a.current {
			quietButton(c, "chat", "返回主会话").OnClick(func() { a.openSession("", conv.rootID) })
		}
		if !a.sidebar {
			iconButton(c, "folder", "打开项目").Disabled(a.opening).OnClick(a.chooseWorkspace)
		}
		if conv != nil {
			if conv.inspector.open {
				a.paneToolbar(c, conv, layout.right.width-14)
			} else if conv.inspector.current != nil {
				iconButton(c, "panel-right", "打开右栏").OnClick(func() { conv.inspector.open = true })
			} else {
				a.paneMenu(c, conv, "panel-right", "打开右栏")
			}
		}
	})
}

func (a *app) banner(c *ui.Context, text string, dismiss func()) {
	ui.Row(c).Margin(0, 24, 8, 24).Padding(10, 12).Gap(10).Background(c.Theme().Surface).Radius(8).Children(func() {
		ui.Text(c, text).FontSize(12).Grow(1).MinWidth(0).Selectable()
		ui.ButtonBase(c).Label("关闭提示").Children(func() { ui.Text(c, "×").FontSize(16) }).OnClick(dismiss)
	})
}

func (a *app) transcript(c *ui.Context, conv *conversation, available float32, showOutline bool) {
	items := mergeTranscript(conv.history, conv.view.Transcript)
	if len(items) == 0 {
		ui.Column(c).Grow(1).Center().Padding(24).Gap(18).Children(func() {
			ui.Icon(c, icons["code"]).Size(32, 32).TextColor(c.Theme().TextMuted)
			ui.Row(c).Gap(8).Children(func() {
				for _, suggestion := range []struct{ label, prompt string }{
					{"了解代码", "介绍这个项目的结构、关键模块和运行方式。"},
					{"检查改动", "检查当前未提交的改动，找出潜在问题并说明原因。"},
					{"实现功能", "我想为这个项目实现一个新功能："},
				} {
					ui.Button(c, suggestion.label).Padding(10, 18).Radius(10).FontSize(12).OnClick(func() { conv.draft = suggestion.prompt })
				}
			})
		})
		return
	}
	if (conv.before == 0 && conv.view.TranscriptMore) || conv.more {
		ui.Button(c, "加载更早的消息").AlignSelf(ui.Center).Disabled(conv.loading).OnClick(func() { a.loadHistory(conv) })
	}
	items = protocol.ChatItems(items)
	prompt := conv.view.TranscriptPrompt
	if len(conv.history) > 0 {
		prompt = conv.historyPrompt
	}
	rows := transcriptRows(items, conv.expanded)
	if prompt != nil && len(items) > 0 && items[0].Kind != "user" && sameTurn(prompt.TurnID, items[0].TurnID) {
		rows = append([]transcriptRow{{item: prompt}}, rows...)
	}
	conv.list.Key = func(i int) any { return rows[i].key() }
	ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		padding := max(float32(18), (available-760)/2)
		leftPadding := padding
		if showOutline {
			leftPadding = max(0, padding-turnOutline(c, conv, rows))
		} else {
			conv.outlineActive = ""
		}
		ui.List(c.Key("transcript-list"), &conv.list, len(rows), func(i int) { a.transcriptRow(c, conv, rows[i]) }).Grow(1).MinWidth(0).Padding(24, padding, 24, leftPadding).Gap(4)
	})
}

func (a *app) message(c *ui.Context, conv *conversation, item protocol.TranscriptItem) {
	t := c.Theme()
	switch item.Kind {
	case "user":
		ui.Row(c).Children(func() {
			ui.Spacer(c)
			ui.Text(c, item.Text).Selectable().FontSize(14).Padding(14, 18).Radius(17).Background(t.Surface).MaxWidth(620).MinWidth(0).Shrink(1)
		})
	case "assistant":
		a.chatMarkdown(c, conv, item.Text, item.Status == "streaming")
	case "collaboration":
		a.collaborationRow(c, conv, item)
	case "tool", "thinking":
		a.activityDetail(c, conv, item)
	case "turn_summary":
		label := activityStatus(item.Status)
		if label == "成功" {
			label = "用时 " + elapsedLabel(item.DurationMs)
		} else if item.DurationMs > 0 {
			label += fmt.Sprintf(" · %.1f 秒", float64(item.DurationMs)/1000)
		}
		ui.Text(c, label).FontSize(11).TextColor(t.TextMuted)
	default:
		if item.Text != "" {
			ui.Text(c, item.Text).Selectable().FontSize(13).TextColor(t.TextMuted)
		}
	}
}

func (a *app) messageField(c *ui.Context, conv *conversation, label, placeholder string) {
	t := c.Theme()
	input := ui.TextArea(c.Key(conv), &conv.draft).Label(label).MinHeight(0).Lines(2, 6).
		Placeholder(placeholder).PlaceholderColor(t.TextMuted.Mix(t.Background, 0.30)).
		Padding(4, 2).FontSize(14).Border(0, ui.Transparent).Background(ui.Transparent).ReadOnly(conv.pending != nil)
	if conv.client == nil {
		input.AutoFocus()
	}
	input.OnTextKey(func(k ui.TextKey) bool {
		if k.Key == ui.KeyEnter && (k.Mods == 0 || k.Mods == ui.Cmd) {
			conv.draft = k.Text()
			a.send(conv)
			return true
		}
		return false
	})
}

func (a *app) composer(c *ui.Context, conv *conversation) {
	t := c.Theme()
	ui.Column(c).Padding(12, 28, 16, 28).Gap(9).Children(func() {
		ui.Column(c).MaxWidth(740).WidthPercent(100).AlignSelf(ui.Center).Gap(8).Children(func() {
			a.decisions(c, conv)
			for _, input := range conv.view.PendingInputs {
				if input.Strategy == protocol.InputMessage {
					continue
				}
				ui.Text(c, "等待处理 · "+shorten(input.Text, 100)).FontSize(12).TextColor(t.TextMuted).Padding(8, 12).Background(t.Surface).Radius(8)
			}
			ui.Column(c).Role(ui.RoleGroup).Label("消息编辑区").Padding(12, 12, 8, 12).Gap(9).Radius(20).Border(1, t.Border).Background(t.Background).Shadow(0, 2, 8, 0, ui.RGBA(0, 0, 0, 0.035)).Children(func() {
				a.messageField(c, conv, "消息", "和 ccdp 一起工作")
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					a.permissionPicker(c, conv)
					ui.Spacer(c)
					usageIndicators(c, conv.view)
					a.modelPicker(c, conv)
					if a.opening {
						ui.Spinner(c).Size(14, 14).Label("正在打开会话")
					}
					busy := conv.executing() || conv.sending && conv.pending != nil
					if conv.pending != nil && !conv.sending {
						ui.Button(c, "重试发送").Disabled(conv.configApplying || conv.configError != "").OnClick(func() { a.send(conv) })
					}
					if busy {
						stop := ui.ButtonBase(c.Key("stop")).Label("停止执行").Tooltip("停止执行").Size(28, 28).Padding(0).Radius(14).Center().Shrink(0).Disabled(conv.sending)
						color := ui.Hex("#3066c9")
						if stop.Hovered() {
							color = ui.Hex("#2657b1")
						}
						stop.Background(color).Children(func() {
							ui.Box(c).Size(9, 9).Radius(1.5).Background(ui.Hex("#ffffff"))
						}).OnClick(func() { a.command(conv, protocol.Command{Type: protocol.CommandInterrupt}) })
					} else if conv.pending == nil {
						ui.PrimaryButton(c, "").Label("发送消息").Tooltip("发送").Size(28, 28).Padding(5).Radius(14).
							Disabled(a.opening || conv.sending || conv.configApplying || conv.configError != "" || strings.TrimSpace(conv.draft) == "").
							Children(func() { ui.Icon(c, icons["send"]).Size(18, 18) }).OnClick(func() { a.send(conv) })
					}
				})
			})
		})
	})
}

func (a *app) decisions(c *ui.Context, conv *conversation) {
	t := c.Theme()
	if approval := conv.view.Approval; approval != nil {
		source := "主会话"
		if root := a.conversations[conv.rootID]; root != nil && root != conv {
			source = conv.title
			if child := childFor(root, conv.view.SessionID, nil); child != nil {
				source = childDetail(child).name
			}
		}
		ui.Column(c.Key(approval.ID)).Padding(14).Gap(9).Radius(12).Border(1, t.Border).Background(t.Surface).Children(func() {
			ui.Text(c, source+" · "+approval.Tool+" 请求批准").FontSize(14).Bold()
			ui.Scroll(c).Height(100).Gap(6).Children(func() {
				ui.Text(c, approval.Reason).FontSize(12).Selectable()
				if len(approval.Args) > 0 {
					ui.Text(c, string(approval.Args)).FontSize(12).Selectable()
				}
				for _, cap := range approval.Capabilities {
					label := cap.Kind + " " + cap.Access
					switch cap.Kind {
					case "directory":
						label = "写入目录 " + cap.Path
					case "local_service":
						label = fmt.Sprintf("%s %s %s:%d", cap.Direction, cap.Protocol, cap.Address, cap.Port)
					}
					ui.Text(c, label).FontSize(12).Selectable()
				}
			})
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "拒绝").Disabled(conv.sending).OnClick(func() {
					a.command(conv, protocol.Command{Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: approval.ID}})
				})
				ui.PrimaryButton(c, "允许一次").Disabled(conv.sending).OnClick(func() {
					a.command(conv, protocol.Command{Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: approval.ID, Approve: true, CapabilityScope: protocol.CapabilityScopeOnce}})
				})
			})
		})
	}
	if question := conv.view.Question; question != nil {
		if conv.questionID != question.ID {
			conv.questionID = question.ID
			conv.answers = make([]protocol.QuestionAnswer, len(question.Questions))
		}
		ui.Column(c.Key(question.ID)).Padding(14).Gap(10).Radius(12).Border(1, t.Border).Background(t.Surface).Children(func() {
			ui.Scroll(c).Height(180).Gap(12).Children(func() {
				for i, q := range question.Questions {
					ui.Column(c.Key(i)).Gap(6).Children(func() {
						ui.Text(c, q.Question).FontSize(13).Bold()
						for _, option := range q.Options {
							selected := slices.Contains(conv.answers[i].Selected, option.Label)
							if ui.Checkbox(c.Key(option.Label), &selected, option.Label).Tooltip(option.Description).Changed() {
								if !q.MultiSelect {
									conv.answers[i].Selected = nil
								}
								if selected {
									conv.answers[i].Selected = append(conv.answers[i].Selected, option.Label)
								} else {
									conv.answers[i].Selected = slices.DeleteFunc(conv.answers[i].Selected, func(v string) bool { return v == option.Label })
								}
							}
						}
						ui.TextInput(c, &conv.answers[i].Text).Label(q.Header + "补充说明").Placeholder("或输入你的回答…").FontSize(12)
					})
				}
			})
			answer := protocol.AnswerQuestion{RequestID: question.ID, Answers: conv.answers}
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "取消回答").Disabled(conv.sending).OnClick(func() {
					a.command(conv, protocol.Command{Type: protocol.CommandAnswerQuestion, Answer: &protocol.AnswerQuestion{RequestID: question.ID, Cancelled: true}})
				})
				ui.PrimaryButton(c, "提交回答").Disabled(conv.sending || question.ValidateAnswer(answer) != nil).OnClick(func() {
					frozen := protocol.CloneAnswer(answer)
					a.command(conv, protocol.Command{Type: protocol.CommandAnswerQuestion, Answer: &frozen})
				})
			})
		})
	}
}
