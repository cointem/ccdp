package desktop

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/mcp"
	"ccdp/internal/protocol"
	"ccdp/internal/skills"

	"github.com/egoist/mygo/ui"
)

type projectExtension struct {
	name, detail           string
	source                 config.Source
	path                   string
	pendingTrust, shadowed bool
}

type projectExtensions struct {
	skills, servers, hooks, tools []projectExtension
	problem, skillsProblem        string
	restoreExa                    bool
}

func loadProjectExtensions(workspace, configPath string) *projectExtensions {
	// The runtime always discovers user skills under ~/.ccdp, including when
	// launched with a different user configuration file.
	userSkills := filepath.Join(filepath.Dir(config.ConfigPath()), "skills")
	return readProjectExtensions(workspace, configPath, userSkills, config.DefaultTrustStore())
}

func readProjectExtensions(workspace, configPath, userSkills string, trust *config.TrustStore) *projectExtensions {
	user, err := config.LoadFrom(configPath)
	result, effective := configuredExtensions(workspace, user, err, trust, false)
	if result.problem != "" {
		return result
	}
	root := effective.Workspace
	store := skills.NewStore()
	if err := store.LoadChecked(effective.Sandbox().ResolveRead, userSkills, filepath.Join(config.ProjectSettingsDir(root), "skills")); err != nil {
		result.skillsProblem = err.Error()
		return result
	}
	for _, skill := range store.All() {
		source := config.SourceUserFile
		if skill.Source == "project" {
			source = config.SourceProject
		}
		result.skills = append(result.skills, projectExtension{name: skill.Name, detail: skill.Description, source: source, path: skill.CanonicalFile})
	}
	return result
}

func configuredExtensions(workspace string, effective config.Config, err error, trust *config.TrustStore, includeShadowed bool) (*projectExtensions, config.Config) {
	result := &projectExtensions{}
	if err != nil {
		result.problem = err.Error()
		return result, effective
	}
	var project config.Config
	var shadowed []projectExtension
	if workspace != "" {
		var root string
		root, err = workspacePath(workspace)
		if err == nil {
			err = effective.ApplyCLIOverride("workspace", root)
		}
		if err == nil {
			project, err = config.LoadProjectSettingsTrusted(root, trust)
		}
		if err != nil {
			result.problem = err.Error()
		} else {
			if includeShadowed && project.SourceReport().ProjectTrusted && project.IsExplicit("mcp_servers") {
				var user projectExtensions
				user.addConfigured(&effective, false)
				shadowed = user.servers
			}
			config.ApplyProjectSettings(&effective, &project)
			effective.TrustStore = trust
		}
	}
	result.addConfigured(&effective, false)
	if workspace != "" && err == nil && !project.SourceReport().ProjectTrusted {
		result.addConfigured(&project, true)
	}
	if scope := effective.SourceOf("mcp_servers"); scope == config.SourceProject || scope == config.SourceProjectLocal {
		for _, entry := range shadowed {
			entry.shadowed = true
			result.servers = append(result.servers, entry)
		}
	}
	return result, effective
}

// Retain only display metadata. Commands, arguments, remote URLs, and
// environment values may contain credentials and do not belong in the list.
func (p *projectExtensions) addConfigured(cfg *config.Config, pending bool) {
	fields := cfg.SourceReport().Fields
	row := func(field, name, detail string) projectExtension {
		src := fields[field]
		return projectExtension{name: name, detail: detail, source: src.Source, path: src.Path, pendingTrust: pending}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
		server := cfg.MCPServers[name]
		transport := server.Transport
		if transport == "" {
			transport = "stdio"
		}
		if len(server.OnDemandTools) > 0 && transport == "http" {
			transport += " · 按需连接"
		}
		if name == mcp.ExaServer && server.IsHostedExa() {
			transport += " · 网页搜索"
			if !cfg.WebToolsEnabled() {
				transport += "已关闭"
			}
		}
		entry := row("mcp_servers", name, transport)
		if server.DefaultInstall {
			entry.source, entry.path = config.SourceDefault, ""
		}
		if server.Disabled {
			entry.detail = "已停用"
		}
		p.servers = append(p.servers, entry)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Hooks)) {
		if n := len(cfg.Hooks[name]); n > 0 {
			p.hooks = append(p.hooks, row("hooks", name, fmt.Sprintf("%d 个动作", n)))
		}
	}
	for _, tool := range cfg.Tools {
		p.tools = append(p.tools, row("tools", tool.Name, tool.Description))
	}
}

func (e projectExtension) scopeLabel() string {
	label := e.sourceLabel()
	if e.pendingTrust {
		label += " · 待信任"
	} else if e.shadowed {
		label += " · 已被项目配置覆盖"
	}
	return label
}

func (e projectExtension) sourceLabel() string {
	switch e.source {
	case config.SourceProject:
		return "项目"
	case config.SourceProjectLocal:
		return "项目本地"
	case config.SourceEnvironment:
		return "环境变量"
	case config.SourceCLI:
		return "启动参数"
	case config.SourceDefault:
		return "默认安装"
	default:
		return "用户"
	}
}

func projectExtensionsView(c *ui.Context, card ui.Color, p *projectExtensions, catalog *protocol.CatalogSnapshot, sessionTitle string, refresh func()) {
	ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
		ui.Text(c, "项目扩展").FontSize(28).FontWeight(650).Grow(1)
		ui.Button(c, "刷新").Label("刷新项目扩展").Disabled(p == nil).OnClick(refresh)
	})
	projectExtensionLists(c, card, p, catalog, sessionTitle)
}

func projectExtensionLists(c *ui.Context, card ui.Color, p *projectExtensions, catalog *protocol.CatalogSnapshot, sessionTitle string) {
	t := c.Theme()
	if p == nil {
		ui.Text(c, "正在读取项目扩展…").TextColor(t.TextMuted)
		return
	}
	if p.problem != "" {
		ui.Text(c, p.problem).TextColor(t.Danger).Selectable()
	}
	list := func(title, empty string, entries []projectExtension) {
		settingsCard(c, card, title, func() {
			if len(entries) == 0 {
				ui.Text(c, empty).FontSize(13).TextColor(t.TextMuted)
				return
			}
			for _, entry := range entries {
				ui.Column(c).Gap(4).Children(func() {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						ui.Text(c, entry.name).FontSize(14).FontWeight(600).Grow(1).MinWidth(0).Selectable()
						ui.Text(c, entry.scopeLabel()).Tooltip(entry.path).FontSize(12).TextColor(t.TextMuted)
					})
					if entry.detail != "" {
						ui.Text(c, entry.detail).FontSize(13).TextColor(t.TextMuted).Selectable()
					}
				})
			}
		})
	}
	if p.problem == "" {
		if p.skillsProblem != "" {
			settingsCard(c, card, "技能", func() {
				ui.Text(c, p.skillsProblem).FontSize(13).TextColor(t.Danger).Selectable()
			})
		} else {
			list("技能", "未发现技能。可在 .ccdp/skills/<名称>/SKILL.md 添加项目技能。", p.skills)
		}
		list("已配置的 MCP 服务", "当前项目和用户配置中均无 MCP 服务。", p.servers)
	}
	settingsCard(c, card, "会话已发布的 MCP 工具", func() {
		var names []string
		if catalog != nil {
			if sessionTitle != "" {
				ui.Text(c, "来自会话「"+sessionTitle+"」").FontSize(12).TextColor(t.TextMuted)
			}
			for _, tool := range catalog.Tools {
				if protocol.IsMCPToolName(tool.ID) {
					names = append(names, tool.ID)
				}
			}
		}
		switch {
		case catalog == nil:
			ui.Text(c, "此项目的会话连接后可查看工具列表。").FontSize(13).TextColor(t.TextMuted)
		case len(names) == 0:
			ui.Text(c, "会话尚未发布 MCP 工具。").FontSize(13).TextColor(t.TextMuted)
		default:
			ui.Text(c, strings.Join(names, "\n")).FontSize(13).Selectable()
		}
	})
	if len(p.tools) > 0 {
		list("已配置的自定义工具", "", p.tools)
	}
	if len(p.hooks) > 0 {
		list("已配置的 Hooks", "", p.hooks)
	}
}
