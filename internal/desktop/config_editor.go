package desktop

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
)

type settingsPage struct {
	form          *configForm
	provider      *providerForm
	general       bool
	discardPrompt bool
	saved         bool
	problem       string
	credentials   map[*providerForm]*credentialFields
}

// Keep each authentication method's draft while switching controls. Only the
// selected method's value belongs to the file-backed form.
type credentialFields struct {
	envMode bool
	key     string
	env     string
}

func (a *app) openSettings() {
	if a.settingsPage != nil {
		return
	}
	form, err := loadConfigForm(a.configPath)
	page := &settingsPage{form: form, credentials: make(map[*providerForm]*credentialFields)}
	a.settingsPage = page
	if err != nil {
		page.problem = err.Error()
		return
	}
	activeID, _, _ := strings.Cut(form.Model, "/")
	for _, p := range form.Providers {
		if page.provider == nil || p.ID == activeID {
			page.provider = p
		}
	}
}

func (p *settingsPage) addProvider() {
	provider := &providerForm{WireAPI: "chat", Models: []*modelForm{{}}}
	p.form.Providers = append(p.form.Providers, provider)
	p.selectProvider(provider)
}

func (p *settingsPage) selectProvider(provider *providerForm) {
	p.provider, p.general = provider, false
}

func providerLabel(p *providerForm) string {
	if p.ID == "" {
		return "新供应商"
	}
	return p.ID
}

func (a *app) settingsView(c *ui.Context) {
	page := a.settingsPage
	t := c.Theme()
	side := ui.Hex("#f1f1ee")
	card := ui.Hex("#ffffff")
	if t.Dark {
		side, card = ui.Hex("#191919"), ui.Hex("#272727")
	}
	width, _ := c.Size()
	width -= navigationRailWidth
	rail := a.settingsSidebar(width)
	pad := max(float32(24), (width-rail.width-1-758)/2)
	rail.split(c.Key("settings-page"), width, false, func() {
		ui.Column(c).Fill().Background(side).Children(func() {
			ui.Column(c).Padding(10, 14, 14, 14).Gap(8).Children(func() {
				ui.Text(c, "设置").FontSize(23).FontWeight(650).Margin(14, 8, 6, 8)
				button := quietButton(c, "settings", "通用").Disabled(page.form == nil)
				if page.general {
					button.Background(t.Border)
				}
				button.OnClick(func() { page.general = true })
				quietButton(c, "provider", "MCP").OnClick(func() { a.navigate(mcpDestination) })
			})
			if page.form == nil {
				return
			}
			ui.Row(c).Padding(0, 14, 0, 22).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "模型供应商").FontSize(12).FontWeight(600).TextColor(t.TextMuted).Grow(1)
				iconButton(c, "new", "添加供应商").OnClick(page.addProvider)
			})
			ui.Scroll(c).Grow(1).MinHeight(0).Padding(2, 14).Gap(3).Children(func() {
				for _, provider := range page.form.Providers {
					row := quietButton(c.Key(provider), "provider", providerLabel(provider))
					if !page.general && page.provider == provider {
						row.Background(t.Border)
					}
					row.OnClick(func() { page.selectProvider(provider) })
				}
			})
		})
	}, func() {
		ui.Column(c).Fill().MinWidth(0).Children(func() {
			if page.form == nil {
				ui.Column(c).Grow(1).Center().Padding(28).Gap(14).Children(func() {
					ui.Text(c, "无法读取配置").FontSize(22).Bold()
					ui.Text(c, page.problem).Selectable()
					ui.Button(c, "重新读取").OnClick(func() { a.settingsPage = nil; a.openSettings() })
				})
				return
			}
			var section any = "general"
			if !page.general && page.provider != nil {
				section = page.provider
			}
			ui.Scroll(c.Key(section)).Grow(1).MinHeight(0).Padding(20, pad, 28, pad).Gap(20).Children(func() {
				switch {
				case page.general:
					page.generalView(c, card, a.configPath)
				case page.provider != nil:
					page.providerView(c, card, page.provider)
				default:
					ui.Text(c, "模型供应商").FontSize(28).FontWeight(650)
					ui.Column(c).Padding(28).Gap(12).Radius(12).Border(1, t.Border).Background(card).Children(func() {
						ui.Text(c, "连接你的第一个供应商").FontSize(18).Bold()
						ui.Text(c, "填入 API 地址和模型名称，即可配置对话使用的模型。").TextColor(t.TextMuted)
						ui.PrimaryButton(c, "连接供应商").AlignSelf(ui.Start).OnClick(page.addProvider)
					})
				}
			})
			page.footer(c, a, pad)
		})
	})
	ui.Modal(c, &page.discardPrompt, func() {
		ui.Column(c).Width(340).Padding(5).Gap(16).Children(func() {
			ui.Text(c, "放弃未保存的更改？").FontSize(19).Bold()
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				ui.Button(c, "放弃更改").OnClick(func() { a.showDestination(a.afterDiscard) })
				ui.PrimaryButton(c, "继续编辑").OnClick(func() { page.discardPrompt = false })
			})
		})
	})
}

func (p *settingsPage) footer(c *ui.Context, a *app, pad float32) {
	t := c.Theme()
	dirty := p.form.dirty()
	if dirty {
		p.saved = false
	}
	ui.Column(c).Padding(12, pad).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Gap(6).Children(func() {
		if p.problem != "" {
			ui.Text(c, p.problem).FontSize(12).TextColor(t.Danger).Selectable()
		}
		ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
			hint := ""
			if dirty {
				hint = "有未保存的更改"
			} else if p.saved {
				hint = "已保存"
			}
			ui.Text(c, hint).FontSize(12).TextColor(t.TextMuted).Grow(1)
			save := func() {
				if !p.form.dirty() {
					return
				}
				if err := p.form.save(a.configPath); err != nil {
					p.problem = err.Error()
					return
				}
				p.problem, p.saved = "", true
				clear(p.credentials)
				a.applySavedConfig(p.form.resolved)
			}
			ui.PrimaryButton(c, "保存配置").Padding(8, 16).Disabled(!dirty).OnClick(save)
			c.OnWindowShortcut(ui.Cmd, ui.KeyS, save)
		})
	})
}

func settingsCard(c *ui.Context, bg ui.Color, title string, fields func()) {
	ui.Column(c).Padding(20).Gap(16).Background(bg).Border(1, c.Theme().Border).Radius(12).Children(func() {
		ui.Text(c, title).FontSize(15).FontWeight(650)
		fields()
	})
}

func field(c *ui.Context, label string, fn func()) {
	ui.Column(c.Key(label)).Grow(1).MinWidth(0).Gap(6).Children(func() {
		ui.Text(c, label).FontSize(12).FontWeight(550).TextColor(c.Theme().TextMuted)
		fn()
	})
}

func textField(c *ui.Context, label, hint string, value *string) {
	field(c, label, func() {
		ui.TextInput(c, value).Label(label + "输入框").Placeholder(hint).FontSize(13).Height(34)
	})
}

func (p *settingsPage) providerView(c *ui.Context, card ui.Color, provider *providerForm) {
	t := c.Theme()
	ui.Column(c).Gap(6).Children(func() {
		ui.Text(c, "模型供应商").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, providerLabel(provider)).SingleLine().FontSize(28).FontWeight(650).Grow(1).MinWidth(0)
			ui.Button(c, "移除供应商").FontSize(12).OnClick(func() {
				p.form.Providers = slices.DeleteFunc(p.form.Providers, func(v *providerForm) bool { return v == provider })
				delete(p.credentials, provider)
				p.provider = nil
				if len(p.form.Providers) > 0 {
					p.provider = p.form.Providers[0]
				}
			})
		})
	})
	settingsCard(c, card, "连接", func() {
		ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
			textField(c, "供应商标识", "例如 my-provider", &provider.ID)
			field(c, "API 协议", func() {
				names := []string{"Chat Completions", "Responses", "Anthropic Messages"}
				values := []string{"chat", "responses", "anthropic"}
				display := names[max(0, slices.Index(values, provider.WireAPI))]
				ui.Select(c, &display, names).Label("API 协议").Height(34).OnChange(func() {
					provider.WireAPI = values[slices.Index(names, display)]
				})
			})
		})
		textField(c, "API 地址", "https://api.example.com/v1", &provider.BaseURL)
		p.authentication(c, provider)
	})
	settingsCard(c, card, "模型", func() {
		for _, model := range provider.Models {
			ui.Column(c.Key(model)).Gap(10).Children(func() {
				ui.Row(c).Gap(10).AlignItems(ui.End).Children(func() {
					textField(c, "模型标识", "服务商提供的模型 ID", &model.ID)
					if len(provider.Models) > 1 {
						ui.Button(c, "移除").Label("移除模型 " + model.ID).OnClick(func() {
							provider.Models = slices.DeleteFunc(provider.Models, func(v *modelForm) bool { return v == model })
						})
					}
				})
				ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
					textField(c, "上下文窗口", "使用默认值", &model.ContextWindow)
					textField(c, "最大输出", "使用默认值", &model.MaxOutputTokens)
				})
			})
		}
		quietButton(c, "new", "添加模型").AlignSelf(ui.Start).OnClick(func() { provider.Models = append(provider.Models, &modelForm{}) })
	})
}

func (p *settingsPage) authentication(c *ui.Context, provider *providerForm) {
	auth := p.credentials[provider]
	if auth == nil {
		auth = &credentialFields{envMode: provider.APIKeyEnv != ""}
		p.credentials[provider] = auth
	}
	ui.Column(c).Gap(8).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
			ui.Text(c, "认证方式").FontSize(12).FontWeight(550).TextColor(c.Theme().TextMuted)
			selected := 0
			if auth.envMode {
				selected = 1
			}
			ui.Segmented(c, &selected, "API 密钥", "环境变量").Label("认证方式").OnChange(func() {
				auth.envMode = selected == 1
				if auth.envMode {
					auth.key, provider.APIKey = provider.APIKey, ""
					provider.APIKeyEnv = auth.env
				} else {
					auth.env, provider.APIKeyEnv = provider.APIKeyEnv, ""
					provider.APIKey = auth.key
				}
			})
		})
		if auth.envMode {
			ui.TextInput(c.Key("environment"), &provider.APIKeyEnv).Label("密钥环境变量输入框").Placeholder("例如 MY_MODEL_API_KEY").Height(34)
		} else {
			ui.TextInput(c.Key("key"), &provider.APIKey).Password().Label("API 密钥输入框").Placeholder("输入 API 密钥，无需认证可留空").Height(34)
		}
	})
}

func (p *settingsPage) generalView(c *ui.Context, card ui.Color, path string) {
	f := p.form
	ui.Text(c, "通用").FontSize(28).FontWeight(650)
	settingsCard(c, card, "对话默认值", func() {
		options := f.modelOptions()
		modelField := func(label, empty, selected string, optional bool, selectModel func(string)) {
			field(c, label, func() {
				candidates := options
				if optional || len(candidates) == 0 {
					candidates = append([]string{empty}, candidates...)
				}
				if selected == "" {
					selected = empty
				}
				if !slices.Contains(candidates, selected) {
					candidates = append(candidates, selected)
				}
				ui.Select(c, &selected, candidates).Label(label).Height(34).OnChange(func() {
					if selected == empty {
						selected = ""
					}
					selectModel(selected)
				})
			})
		}
		modelField("默认模型", "使用默认值", f.defaultModel(), false, f.selectDefaultModel)
		modelField("备用模型", "不使用备用模型", f.fallbackModel(), true, f.selectFallbackModel)
		field(c, "权限模式", func() {
			mode := 0
			if f.PermissionMode == "manual" {
				mode = 1
			}
			ui.Segmented(c, &mode, "auto", "manual").Label("权限模式").AlignSelf(ui.Start).OnChange(func() {
				f.PermissionMode = []string{"auto", "manual"}[mode]
			})
		})
	})
	settingsCard(c, card, "配置文件", func() {
		ui.Text(c, path).Selectable().FontSize(13).TextColor(c.Theme().TextMuted)
	})
}
