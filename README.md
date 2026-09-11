# ccdp

一个用 Go 编写的终端 Coding Agent：**Codex 式事件驱动循环 + Claude Code 式交互体验**。通过任意 OpenAI 兼容协议（`base_url` + `api_key` + `model`）驱动，自带 TUI、权限门禁、沙箱、MCP、会话持久化与上下文压缩。

```
ccdp 0.2.0 · Go 1.27+ · 无第三方 LLM SDK 依赖（纯 HTTP 流式实现）
```

## 功能特性

- **Agent 循环** — Infer → ToolDispatch → ApprovalGate → Compact，支持并行工具执行（`max_parallel_tools`）、工具结果聚合预算、截断回复自动续写、`--max-turns` / `--max-budget` 双重熔断
- **实时 TUI**（bubbletea）— 工具执行三粒度展示（启动提示 / 流式输出 / 完成摘要）、状态栏（模型 / 模式 / 花费 / 上下文占用）、多行输入、转录滚动
- **权限体系** — `default` / `acceptEdits` / `plan` / `bypassPermissions` 四种模式；always_allow / always_deny 规则（glob）、会话级审批记忆、turn 级审批缓存（并行同调用不重复弹窗）
- **沙箱** — `confine`（路径围栏）/ `strict`（macOS sandbox-exec 内核隔离 + ulimit 资源限制 + 网络阻断）/ `none`；Bash 与 ProcessStart 同等受限
- **上下文管理** — 结构化自动压缩（6 段式摘要 + 最近读取文件重附 + trace 逃生通道）、token 原生用量锚定估算、`/fork` 会话分支（废弃方向自动摘要）、`/rewind` / `/remove`、checkpoint
- **会话** — JSONL 全事件 trace、崩溃安全的 pending inbox（排队消息不丢）、`-r` 恢复、`-c` 会话选择器、`--replay` 导出 Markdown 回放
- **扩展** — MCP 客户端（stdio / SSE / Streamable HTTP，热刷新与断连清理）、自定义 shell 工具、Claude 风格 hooks、自定义斜杠命令、skills、Go 插件系统（依赖拓扑加载 / 级联卸载）
- **LLM 兼容** — OpenAI 协议默认，DeepSeek / Kimi / 通义 / 本地 Ollama、vLLM 等改 `base_url` 即用；`providers` 表按模型路由多供应商；`fallback_model` 故障兜底；prompt cache 感知计费（`/cost`）

## 构建

```bash
git clone https://github.com/cointem/ccdp.git
cd ccdp
go build -o ccdp ./cmd/ccdp
```

## 快速开始

```bash
# 1. 生成配置模板
./ccdp --init

# 2. 编辑 ~/.ccdp/config.json，填入任意 OpenAI 兼容端点
#    （也可用环境变量 CCDP_API_KEY 提供 api_key）

# 3. 运行
./ccdp                    # 在当前目录打开 TUI
./ccdp -d ~/project       # 指定工作目录
./ccdp -m deepseek-chat   # 临时切换模型
```

最小配置：

```json
{
  "api_key": "sk-...",
  "base_url": "https://api.deepseek.com/v1",
  "model": "deepseek-chat"
}
```

> ccdp 需要 LLM API 才能工作；其余能力（工具、权限、沙箱等）无需额外配置。

## 常用命令行

```bash
ccdp -c                          # 打开会话选择器，继续历史会话
ccdp -r <session-id>             # 按 id 恢复会话
ccdp -l                          # 列出已保存的会话
ccdp --replay <session-id>       # 以 Markdown 回放一个会话

# Headless（脚本 / CI 集成，审批自动拒绝）
ccdp -p "修复所有 go vet 告警"                # 单条提示，打印结果
ccdp -p "..." --output-format stream-json    # text | json | stream-json
ccdp --input-format stream-json < input.ndjson

# 安全阀
ccdp --max-turns 20              # 每 turn 模型调用次数上限
ccdp --max-budget 2.0            # 会话硬性花费上限（USD）
ccdp --mode acceptEdits          # default | acceptEdits | bypassPermissions
ccdp --fallback-model gpt-4o     # 主模型失败时的兜底
ccdp --add-dir /data             # 额外授权沙箱目录
ccdp --allowedTools Bash,Read    # 跳过审批门的工具
```

其余 flags：`--system-prompt[-file]`、`--append-system-prompt`、`--disallowedTools`、`--session-id`、`--no-session-persistence`、`--timeout`（headless 超时秒数）、`--verbose` / `--debug`、`--config-file`。`ccdp --version` 查看版本。

## 配置参考（`~/.ccdp/config.json`）

```jsonc
{
  "api_key": "sk-...",
  "base_url": "https://api.example.com/v1",
  "model": "gpt-4.1",

  // 多供应商：按模型路由（省略 api_key 时继承顶层）
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "sk-...",
      "models": ["deepseek-chat", "deepseek-reasoner"]
    }
  },

  // 计费（USD/百万 token），/cost 与 --max-budget 依赖它
  "pricing": { "deepseek-chat": { "input_per_million": 0.27, "output_per_million": 1.1, "context_window": 128000 } },

  "permission_mode": "default",
  "always_allow": ["Bash:git status*"],
  "always_deny":  ["Bash:git push*", "Write:/etc/**"],

  // 上下文管理
  "context_window": 200000,
  "compact_threshold": 0.8,        // 达到窗口比例自动压缩
  "keep_after_compact": 12,        // 压缩后保留最近消息数
  "max_reply_tokens": 8192,        // 单次回复上限，截断自动续写
  "max_tool_output_chars_per_turn": 200000,

  "sandbox_mode": "confine",       // confine | strict | none
  "sandbox_allow_network": false,  // strict 模式下放行网络
  "sandbox_limits": { "cpu_seconds": 600, "memory_mb": 2048 },
  "additional_directories": ["/data"],
  "disallowed_directories": ["~/.ssh"],
  "max_parallel_tools": 4,         // 0 = 顺序执行
  "bash_timeout_seconds": 120,

  "fallback_model": "gpt-4o",
  "max_turns": 0,                  // 每 turn 模型调用上限（0 = 不限）
  "max_budget_usd": 0,             // 会话花费上限（0 = 不限）

  "enable_web_tools": true,        // WebFetch / WebSearch
  "enable_memory": false,          // AutoMem 会话记忆注入
  "enable_guardian": false,        // 高危调用先经只读子代理审查

  // 自定义工具：模型调用时参数以 JSON 走 stdin，stdout 即结果
  "tools": [
    {
      "name": "deploy",
      "description": "部署当前服务",
      "command": "/usr/local/bin/deploy.sh",
      "permission": "ask"            // allow | deny | ask
    }
  ],

  // MCP 服务器：stdio（默认）或 sse
  "mcp_servers": {
    "github": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": { "GITHUB_TOKEN": "..." } },
    "remote": { "transport": "sse", "base_url": "http://localhost:8080/sse" }
  },

  // Hooks（Claude 风格）：简单形式或 matcher 结构形式
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash|Write", "hooks": [{ "command": "./guard.sh", "timeout": 30 }] }
    ],
    "PostToolUse": ["./notify.sh"],
    "UserPromptSubmit": ["./inject-context.sh"]
  },

  "session_dir": "~/.ccdp/sessions"
}
```

项目级覆盖（`.ccdp/settings.json`，`/reload` 热加载）：`permission_mode`、`always_allow` / `always_deny`、`hooks`、`sandbox_mode`、`enable_web_tools`。

## 沙箱模式

| 模式 | 行为 |
|------|------|
| `confine`（默认） | 路径围栏：写操作限制在 workspace 内（symlink 解析后判定），读不受限 |
| `strict` | confine + 读限制 + macOS sandbox-exec 内核隔离 + ulimit 资源限制 + 网络客户端阻断（`sandbox_allow_network: true` 可放行）；Bash 与 ProcessStart 同等受限 |
| `none` | 无沙箱（仅权限门禁生效） |

## 权限模式

| 模式 | 行为 |
|------|------|
| `default` | 写文件、非只读 Bash 等需要审批（y/n，`a` 本会话总是允许） |
| `acceptEdits` | 自动接受文件编辑，Bash 等仍需审批 |
| `plan` | 只读模式：模型只能产出计划，批准后执行 |
| `bypassPermissions` | 全部放行（风险自负） |

规则匹配支持 bare 命令（`"go test *"`）与工具前缀 glob（`"Bash:git status*"`、`"Write:/tmp/*.log"`）；含 `/` 的路径规则中 `*` 不跨越目录分隔符、`**` 跨越。deny 永远优先于 allow。

## TUI 命令与按键

常用斜杠命令（`/help` 查看全部）：

```
/model <name>   切换模型          /mode <mode>      切换权限模式
/sandbox <m>    切换沙箱          /compact          立即压缩上下文
/cost           token 用量与花费   /doctor           环境自检
/rewind [n]     回退历史          /fork [n]         分支新会话
/checkpoint     创建/恢复检查点    /diff /git        查看/执行 git
/sessions       会话列表          /resume <id>      恢复会话
/mcp /plugins   MCP 与插件状态     /skills           技能列表
/permissions    权限规则管理       /add-dir <dir>    扩展沙箱目录
/export [path]  导出 Markdown     /config set <k> <v>
```

| 按键 | 作用 |
|------|------|
| `Enter` / `Alt+Enter` | 发送 / 换行（`Ctrl+J` 同效） |
| `Ctrl+C` | 中断 agent；空闲时按两次退出 |
| `pgup` / `pgdn` / `home` / `end` | 滚动转录 |
| `Ctrl+P` / `Ctrl+N` | 输入历史 |
| `y` / `n` / `a` / `x` | 审批：允许 / 拒绝 / 会话总是允许 / 总是拒绝 |

## 扩展机制

- **自定义斜杠命令** — `~/.ccdp/commands/<name>.md` 或 `.ccdp/commands/<name>.md` 即成为 `/name`（项目优先）；`$ARGUMENTS` 替换为参数
- **Skills** — `~/.ccdp/skills/<name>/SKILL.md` 与 `.ccdp/skills/`（项目优先），经 `ReadSkill` 工具按需加载
- **MCP** — 服务器广告的工具自动注册；`tools/list_changed` 热刷新，断连自动清理；`ToolSearch` 延迟发现不占用内联 schema
- **Hooks** — `PreToolUse` / `PostToolUse` / `UserPromptSubmit` / `PreCompact` / `PostCompact` / `SessionStart` / `Stop` 等事件；退出码 2 = 阻断，`{"decision": "ask"}` 升级审批，`{"continue": false}` 停止会话循环
- **插件（Go API）** — `plugin.Host` 依赖拓扑加载、失败回滚、级联卸载；可注册工具、LLM Provider、Go hooks 与会话回调

## 目录结构

```
cmd/ccdp/          入口：TUI / headless / flags
internal/
  agent/           agent 循环、压缩、会话、分支、子代理、guardian
  tui/             bubbletea 界面、斜杠命令、渲染与清洗
  tools/           内置工具（Bash/文件/搜索/git/进程/Task/Web…）
  llm/             OpenAI 兼容流式客户端
  mcp/             JSON-RPC over stdio/SSE 的 MCP 客户端
  permissions/     权限规则与审批判定
  sandbox/         沙箱（路径围栏 / sandbox-exec / ulimit）
  hooks/           shell hooks 执行器
  plugin/          插件 Host、ModelRegistry、GoHooks
  config/          配置加载与合并
  sessions/trace/  会话持久化与 JSONL trace（checkpoint/events/…）
```

## 安全说明

- API key 仅从环境变量 / config 运行时读取，不入库；`.gitignore` 已覆盖 `.ccdp/`、构建产物、`.env`
- 默认模式下所有写操作与非只读命令需人工审批；`always_deny` 优先级最高
- strict 沙箱在 macOS 上提供内核级隔离；Linux 上为 ulimit + 用户态检查（纵深较弱，请按需选择模式）
- headless 模式（`-p`）审批自动拒绝，适合 CI 只读场景
