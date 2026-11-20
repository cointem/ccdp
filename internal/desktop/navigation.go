package desktop

import "github.com/egoist/mygo/ui"

const navigationRailWidth float32 = 52
const projectSidebarWidth float32 = 246

type primaryDestination uint8

const (
	chatsDestination primaryDestination = iota
	customizationDestination
	settingsDestination
	mcpDestination
)

func (a *app) destination() primaryDestination {
	switch {
	case a.pluginsPage != nil:
		return customizationDestination
	case a.settingsPage != nil:
		return settingsDestination
	default:
		return chatsDestination
	}
}

func (a *app) showingChat() bool {
	return a.destination() == chatsDestination && a.projectPage == nil
}

func (a *app) navigate(to primaryDestination) {
	if a.opening || (to == a.destination() && a.projectPage == nil) {
		return
	}
	a.afterDiscard = to
	if p := a.projectPage; p != nil && p.dirty() {
		p.discard = true
		return
	}
	if p := a.settingsPage; p != nil && p.form != nil && p.form.dirty() {
		p.discardPrompt = true
		return
	}
	a.showDestination(to)
}

func (a *app) showDestination(to primaryDestination) {
	path := a.workspace
	if p := a.projectPage; p != nil {
		path = p.path
	}
	a.projectPage, a.settingsPage, a.pluginsPage = nil, nil, nil
	a.afterDiscard = chatsDestination
	switch to {
	case customizationDestination, mcpDestination:
		a.openCustomization(path)
		if to == mcpDestination {
			a.pluginsPage.tab = mcpTab
		}
	case settingsDestination:
		a.openSettings()
	}
}

func windowChromeColor(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.Hex("#202020")
	}
	return ui.Hex("#f6f6f4")
}

func (a *app) navigationRail(c *ui.Context) {
	t := c.Theme()
	button := func(destination primaryDestination, glyph, label string) {
		active := a.destination() == destination
		b := ui.ButtonBase(c.Key(label)).Label(label).Tooltip(label).Size(36, 36).Center().Radius(9).Disabled(a.opening)
		color := t.TextMuted
		if active {
			b.Background(t.Border).Current(ui.CurrentPage)
			color = t.Text
		} else if b.Hovered() {
			b.Background(t.SurfaceHover)
			color = t.Text
		}
		b.Children(func() { ui.Icon(c, icons[glyph]).Size(20, 20).TextColor(color) }).
			OnClick(func() { a.navigate(destination) })
	}
	ui.Column(c.Key("main-navigation")).Width(navigationRailWidth).AlignItems(ui.Center).Gap(6).Padding(8, 0, 12, 0).
		Background(windowChromeColor(t)).BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		button(chatsDestination, "home", "对话")
		button(customizationDestination, "customize", "自定义")
		ui.Spacer(c)
		button(settingsDestination, "settings", "设置")
	})
}
