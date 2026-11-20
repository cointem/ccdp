package desktop

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

var effortLabels = map[string]string{
	"none": "关闭", "minimal": "极低", "low": "低", "medium": "中", "high": "高", "xhigh": "极高", "max": "最高",
}

// Keep the current session setting when possible. Only supply a model override
// when the new model explicitly advertises an incompatible set of levels.
func compatibleEffort(levels []string, current string) string {
	if len(levels) == 0 || slices.Contains(levels, current) {
		return ""
	}
	if slices.Contains(levels, config.DefaultReasoningEffort) {
		return config.DefaultReasoningEffort
	}
	return levels[0]
}

func composerPanel(c *ui.Context, panel ui.Element, width float32) {
	t := c.Theme()
	panel.Width(width).Padding(7).Radius(14).Border(1, t.Border).Background(t.Background).Shadow(0, 5, 18, 0, ui.RGBA(0, 0, 0, 0.12))
}

func (a *app) permissionPicker(c *ui.Context, conv *conversation) {
	mode := conv.view.Settings.Permission.Mode
	if mode == "" {
		return
	}
	t := c.Theme()
	picker := ui.SelectBase(c.Key("permission"), &mode)
	button := picker.Trigger.Label("权限模式").MinWidth(88).Height(28).Padding(4, 9).Gap(5).AlignItems(ui.Center).Radius(14).
		Disabled(a.opening || conv.sending)
	fill := t.Surface
	if button.Hovered() {
		fill = t.SurfaceHover
	}
	button.Background(fill).TextColor(t.TextMuted).Children(func() {
		ui.Icon(c, icons[mode]).Size(15, 15)
		ui.Text(c, mode).SingleLine().FontSize(12)
		ui.Icon(c, icons["down"]).Size(11, 11)
	})
	picker.Popup(func(panel ui.Element) {
		composerPanel(c, panel, 268)
		panel.Label("审批模式").AttachTo(button, ui.AnchorTopStart, ui.AnchorBottomStart).Margin(0, 0, 5, 0)
		ui.Text(c, "审批模式").Padding(4, 8, 6, 8).FontSize(11).TextColor(t.TextMuted)
		for _, opt := range []struct{ id, detail string }{
			{"auto", "在已授权范围内自动执行"},
			{"manual", "修改文件或运行非只读命令前询问"},
		} {
			row := picker.Item(opt.id).Label(opt.id).Description(opt.detail).Row().AlignItems(ui.Center).Padding(8).Gap(10).Radius(8)
			if row.Highlighted() {
				row.Background(t.SurfaceHover)
			}
			row.Children(func() {
				ui.Icon(c, icons[opt.id]).Size(17, 17).TextColor(t.TextMuted)
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, opt.id).FontSize(13).FontWeight(600)
					ui.Text(c, opt.detail).FontSize(12).TextColor(t.TextMuted)
				})
				if mode == opt.id {
					ui.Icon(c, icons["check"]).Size(16, 16)
				}
			})
		}
	})
	button.OnChange(func() {
		if conv.client == nil {
			conv.draftMode, conv.view.Settings.Permission.Mode = mode, mode
		} else {
			a.command(conv, modeCommand(conv.view, mode))
		}
	})
}

func (a *app) modelPicker(c *ui.Context, conv *conversation) {
	t := c.Theme()
	settings := &conv.view.Settings
	model, levels := settings.Model.Model, settings.ReasoningEfforts
	trigger := ui.ButtonBase(c.Key("model")).Role(ui.RolePopUpButton).HasPopup(ui.PopupDialog).Label("模型").Value(model).
		MaxWidth(245).MinWidth(0).Shrink(1).Height(28).Gap(4).Padding(4, 7).Radius(14)
	type pickerState struct {
		open, list, pressed, preview bool
		position                     float64
	}
	state := ui.Local(trigger, model, func() pickerState { return pickerState{} })
	saving := conv.sending || conv.view.Revision.SettingsRev < conv.pendingSettingsRev
	trigger.Disabled(a.opening || saving && !state.open)
	if conv.view.Busy {
		trigger.Tooltip("更改将从下一次模型请求生效")
	}
	if trigger.Clicked() {
		state.open = !state.open
		state.list, state.pressed, state.preview = len(levels) == 0, false, false
	}
	if a.opening {
		state.open = false
	}
	trigger.Expanded(state.open)
	if trigger.Hovered() || state.open {
		trigger.Background(t.Surface)
	}
	trigger.Children(func() {
		ui.Text(c, model).SingleLine().MinWidth(0).Shrink(1).FontSize(12)
		if settings.ReasoningEffortSupported {
			ui.Text(c, effortLabels[settings.ReasoningEffort]).SingleLine().Shrink(0).FontSize(12).TextColor(t.TextMuted)
		}
		ui.Icon(c, icons["down"]).Size(11, 11).Shrink(0).TextColor(t.TextMuted)
	})
	ui.PopoverBase(c, trigger, &state.open, func(panel ui.Element) {
		composerPanel(c, panel, 260)
		panel.AttachTo(trigger, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, 0, 5, 0)
		if state.list || len(levels) == 0 {
			ui.Text(c, "模型").Padding(5, 8).FontSize(11).TextColor(t.TextMuted)
			models := a.host.settings.Load().models
			if conv.client != nil {
				models = []string{model}
				for _, p := range conv.view.Catalog.Providers {
					for _, name := range p.Models {
						id := pickerModel(p.ID, name)
						if !slices.Contains(models, id) {
							models = append(models, id)
						}
					}
				}
			}
			ui.Scroll(c).MaxHeight(240).FocusGroup(ui.Vertical).Children(func() {
				for _, name := range models {
					row := ui.ButtonBase(c.Key(name)).Label(name).Padding(7, 8).Radius(7).Gap(6).Disabled(saving)
					if row.Hovered() || row.Focused() {
						row.Background(t.SurfaceHover)
					}
					if name == model {
						row.AutoFocus()
					}
					row.Children(func() {
						ui.Text(c, name).SingleLine().Grow(1).MinWidth(0).FontSize(13)
						if name == model {
							ui.Icon(c, icons["check"]).Size(15, 15)
						}
					}).OnClick(func() {
						state.open = false
						if name == model {
							return
						}
						if conv.client == nil {
							conv.draftModel = name
							a.updateDraftSettings(conv)
						} else {
							override := compatibleEffort(a.host.settings.Load().ReasoningEffortsFor(name), settings.ReasoningEffort)
							a.command(conv, protocol.Command{Type: protocol.CommandSetModel, Model: &protocol.SetModel{Model: name, ReasoningEffort: override}})
						}
					})
				}
			})
			return
		}
		// While the panel stays open, follow confirmed session changes unless the
		// user is previewing a stop or the session is acknowledging their choice.
		if !saving && !state.preview {
			state.position = float64(max(0, slices.Index(levels, settings.ReasoningEffort)))
		}
		state.position = max(0, min(state.position, float64(len(levels)-1)))
		index := int(state.position)
		pick := func(index int) {
			state.preview = false
			effort := levels[index]
			if effort == settings.ReasoningEffort {
				return
			}
			if conv.client == nil {
				conv.draftEffort = effort
				a.updateDraftSettings(conv)
			} else {
				a.command(conv, protocol.Command{Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}})
			}
		}
		ui.Column(c).AlignItems(ui.Center).Gap(3).Padding(4, 6).Children(func() {
			ui.Text(c, effortLabels[levels[index]]).FontSize(16).FontWeight(600).TextColor(sliderAccent(t))
			modelButton := ui.ButtonBase(c).Label("切换模型").Radius(6).Padding(3, 6).Gap(4).Disabled(saving)
			if modelButton.Hovered() {
				modelButton.Background(t.Surface)
			}
			modelButton.Children(func() {
				ui.Text(c, model).SingleLine().Shrink(1).MinWidth(0).FontSize(12).TextColor(t.TextMuted)
				ui.Icon(c, icons["right"]).Size(11, 11).TextColor(t.TextMuted)
			}).OnClick(func() { state.list = true })
			if len(levels) == 1 {
				if settings.ReasoningEffort == levels[0] {
					return
				}
				ui.Button(c, effortLabels[levels[0]]).Label("使用" + effortLabels[levels[0]] + "档").Disabled(saving).OnClick(func() { pick(0) })
				return
			}
			slider := ui.SliderBase(c.Key("effort"), &state.position, 0, float64(len(levels)-1)).Step(1).WidthPercent(100).Height(30).
				PaddingX(12).Margin(4, 0, 0, 0).Label("推理强度").Value(effortLabels[levels[index]]).AutoFocus().Disabled(saving)
			slider.Draw(func(p *ui.Painter, r ui.Rect) { drawEffortScale(p, r, t, len(levels), state.position) })
			// Pointer and keyboard changes preview locally. Commit once on release
			// or Enter, so a long drag never races a series of settings commands.
			pressed := slider.Pressed()
			if slider.Changed() || pressed {
				state.preview = true
			}
			if slider.PressCanceled() {
				state.preview = false
			}
			released := state.pressed && !pressed && !slider.PressCanceled()
			state.pressed = pressed
			if released || slider.Shortcut(0, ui.KeyEnter) {
				pick(int(state.position))
			}
		})
	})
}

func sliderAccent(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.Hex("#6aa4ff")
	}
	return ui.Hex("#357de8")
}

func drawEffortScale(p *ui.Painter, r ui.Rect, t *ui.Theme, count int, value float64) {
	const thumb float32 = 22
	left, right, mid := r.X+12, r.X+r.W-12, r.Y+r.H/2
	x := left + (right-left)*float32(value)/float32(count-1)
	track := ui.Rect{X: r.X + 1, Y: mid - 8, W: r.W - 2, H: 16}
	p.Fill(track, t.Border, 8)
	p.Fill(ui.Rect{X: track.X, Y: track.Y, W: x - track.X + 2, H: 16}, sliderAccent(t), 8)
	for i := 0; i < count; i++ {
		stop := left + (right-left)*float32(i)/float32(count-1)
		color := t.TextMuted.Alpha(.52)
		if stop <= x {
			color = ui.Hex("#ffffff").Alpha(.6)
		}
		p.Fill(ui.Rect{X: stop - 1.6, Y: mid - 1.6, W: 3.2, H: 3.2}, color, 1.6)
	}
	knob := ui.Rect{X: x - thumb/2, Y: mid - thumb/2, W: thumb, H: thumb}
	p.Fill(knob, ui.Hex("#ffffff"), thumb/2)
	p.Stroke(knob, ui.RGBA(0, 0, 0, .18), thumb/2, 1)
}

func tokenCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(max(n, 0))
	}
	return strings.TrimSuffix(strconv.FormatFloat(math.Round(float64(n)/100)/10, 'f', 1, 64), ".0") + "k"
}

func usageIndicators(c *ui.Context, view protocol.SessionView) {
	t := c.Theme()
	limit := view.Settings.ContextWindow
	if limit > 0 {
		used := max(view.ContextUsedTokens, 0)
		tip := "Context · " + tokenCount(used) + " / " + tokenCount(limit) + " tokens"
		ui.Box(c.Key("context-usage")).Size(24, 26).Shrink(0).Focusable().Role(ui.RoleProgress).Label("Context").Range(0, float64(limit), float64(min(used, limit))).Tooltip(tip).
			Draw(func(p *ui.Painter, r ui.Rect) {
				var ring ui.Path
				ring.Circle(r.X+r.W/2, r.Y+r.H/2, 6.5)
				p.StrokePath(&ring, 2.6, t.TextMuted.Alpha(.3))
				if used > 0 {
					p.Transform(ui.Rotation(-90), r.X+r.W/2, r.Y+r.H/2, func() { p.StrokePath(ring.Trim(0, float32(min(used, limit))/float32(limit)), 2.6, t.Text) })
				}
			})
	}
	cache := view.Usage.Cache
	if cache.InputTokens <= 0 && view.Usage.InputTokens <= 0 && view.Usage.TurnCount == 0 {
		return
	}
	reported := cache.Tracking && !cache.UnknownHistory && cache.InputTokens > 0 && cache.ReportedInputTokens == cache.InputTokens && cache.CachedTokens >= 0 && cache.CachedTokens <= cache.InputTokens
	tip := "Session cache read · unavailable"
	color := t.TextMuted.Alpha(.6)
	if reported {
		tip = "Session cache read · " + tokenCount(cache.CachedTokens) + " / " + tokenCount(cache.InputTokens) + " input tokens"
		if cache.CachedTokens > 0 {
			color = t.TextMuted
		}
	}
	ui.Box(c.Key("cache-usage")).Size(22, 26).Shrink(0).Center().Focusable().Role(ui.RoleImage).Label("Cache read").Tooltip(tip).
		Children(func() { ui.Icon(c, icons["cache"]).Size(15, 15).TextColor(color) })
}
