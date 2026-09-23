package plugin

import (
	"ccdp/internal/llm"
	"ccdp/internal/tools"
)

// ---------- Built-in toolset plugin ----------

// NewToolsPlugin returns the plugin that registers the standard toolset. Web
// tools are included only when enableWeb is set, mirroring the
// enable_web_tools config flag.
func NewToolsPlugin(enableWeb bool) Plugin {
	return &toolsPlugin{enableWeb: enableWeb}
}

type toolsPlugin struct{ enableWeb bool }

func (p *toolsPlugin) Name() string       { return "builtin-tools" }
func (p *toolsPlugin) Requires() []string { return nil }
func (p *toolsPlugin) Init(ctx *Context) error {
	builtins := []tools.Tool{
		tools.NewBashTool(),
		tools.NewReadTool(),
		tools.NewWriteTool(),
		tools.NewEditTool(),
		tools.NewGlobTool(),
		tools.NewGrepTool(),
		tools.NewLSTool(),
		tools.NewTodoWriteTool(),
		tools.NewTaskTool(),
		tools.NewAgentTool(),
		tools.NewReadSkillTool(),
		tools.NewGitStatusTool(),
		tools.NewGitDiffTool(),
		tools.NewGitLogTool(),
		tools.NewGitCommitTool(),
		tools.NewProcessStartTool(),
		tools.NewProcessWriteTool(),
		tools.NewProcessOutputTool(),
		tools.NewProcessStopTool(),
	}
	if p.enableWeb {
		ctx.Tools.RegisterIn("web", tools.NewWebFetchTool())
		ctx.Tools.RegisterIn("web", tools.NewWebSearchTool())
	}
	for _, t := range builtins {
		ctx.Tools.RegisterIn("builtin", t)
	}
	return nil
}

func (p *toolsPlugin) Deinit() error { return nil }

// ---------- Default LLM provider plugin ----------

// NewProviderPlugin registers a single LLM provider and routes its model name
// to it. Third-party providers simply provide another Plugin that calls
// ctx.Models.Register at Init.
func NewProviderPlugin(p llm.Provider) Plugin {
	return &providerPlugin{provider: p}
}

// NewHTTPProviderPlugin registers the built-in compatible HTTP adapter while
// retaining the model's endpoint/key/wire fingerprint as a generated route.
// This keeps a reload from reusing a stale adapter, while generic plugin
// providers continue to use explicit routes through NewProviderPlugin.
func NewHTTPProviderPlugin(p llm.Provider, model string, binding HTTPBinding) Plugin {
	return &providerPlugin{provider: p, defaultRoute: true, model: model, binding: binding}
}

type providerPlugin struct {
	provider     llm.Provider
	dispose      func()
	defaultRoute bool
	model        string
	binding      HTTPBinding
}

func (p *providerPlugin) Name() string       { return "default-provider" }
func (p *providerPlugin) Requires() []string { return nil }
func (p *providerPlugin) Init(ctx *Context) error {
	if p.provider == nil || p.provider.Name() == "" {
		return nil
	}
	p.dispose = ctx.Models.Register(p.provider)
	if p.defaultRoute {
		model := p.model
		if model == "" {
			model = p.provider.Name()
		}
		ctx.Models.RouteDefault(model, p.provider.Name(), p.binding)
	} else {
		ctx.Models.Route(p.provider.Name(), p.provider.Name())
	}
	return nil
}

func (p *providerPlugin) Deinit() error {
	if p.dispose != nil {
		p.dispose()
		p.dispose = nil
	}
	return nil
}
