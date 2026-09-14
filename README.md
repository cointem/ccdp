# ccdp

一个用 Go 编写的终端 Coding Agent：**Codex 式事件驱动循环 + Claude Code 式交互体验**。通过任意 OpenAI 兼容协议（`base_url` + `api_key` + `model`）驱动，自带 TUI、权限门禁、沙箱、MCP、会话持久化与上下文压缩。

架构设计与实施：[目标架构](docs/architecture.md) · [参考项目与取舍](docs/architecture-references.md) · [迁移计划](docs/architecture-migration.md) · [集成验收与实际边界](docs/architecture-completion.md)。历史分批记录不代表当前状态；实际实现、测试与原生终端验收限制以集成验收为准。

```
ccdp 0.2.0 · Go 1.27+ · 无第三方 LLM SDK 依赖（纯 HTTP 流式实现）
```

## 功能特性

- **Agent 循环** — Infer → ToolDispatch → ApprovalGate → Compact，支持并行工具执行（`max_parallel_tools`）、工具结果聚合预算、截断回复自动续写、`--max-turns` / `--max-budget` 双重熔断
- **实时 TUI**（Bubble Tea）— Go Gopher 欢迎区、适配深浅终端的青蓝主题、动态工作指示与耗时、紧凑工具结果；默认 inline 转录、共享可滚动选择器、状态栏、多行输入和原文 `/copy`；普通聊天不捕获鼠标，历史滚动与拖选交给终端
- **需求澄清** — `AskUserQuestion` 支持 1–4 题、单选/多选和自由补充；plan 模式可用，答案不代替执行审批
- **推理与输出控制** — `reasoning_effort` / `verbosity` 支持配置、环境变量、CLI 和 `/effort` / `/verbosity`；会话保存与恢复
- **权限体系** — `default` / `acceptEdits` / `bypassPermissions` 审批策略与独立 plan 工作流；兼容 `/mode plan`，always_allow / always_deny 规则（glob）、会话级审批记忆、turn 级审批缓存
- **沙箱** — `confine`（路径围栏）/ `strict`（macOS sandbox-exec 内核隔离 + ulimit 资源限制 + 网络阻断）/ `none`；Bash 与 ProcessStart 同等受限
- **上下文管理** — 结构化自动压缩（6 段式摘要 + 最近读取文件重附 + trace 逃生通道）、token 原生用量锚定估算、`/fork` 会话分支（废弃方向自动摘要）、`/rewind` / `/remove`、checkpoint
- **会话** — 事务 JSONL 存储、持久化 inbox、稳定输入 ID、冻结请求 manifest 与附件 blob、工具副作用事实与 unknown 恢复；旧 JSON 只读导入、`-r` 恢复、`-c` 选择器、`--replay` 只读导出 Markdown。恢复不自动重跑结果未知的副作用
- **扩展** — MCP 客户端（stdio / SSE / Streamable HTTP，热刷新与断连清理）、自定义 shell 工具、Claude 风格 hooks、自定义斜杠命令、skills、Go 插件系统（依赖拓扑加载 / 级联卸载）
- **LLM 兼容** — OpenAI 协议默认，DeepSeek / Kimi / 通义 / 本地 Ollama、vLLM 等改 `base_url` 即用；`providers` 表按模型路由多供应商；`fallback_model` 故障兜底；prompt cache 感知计费（`/cost`）

## 需求澄清与模型参数

模型可调用 `AskUserQuestion` 提出 1–4 个问题，每题提供 2–4 个选项。`↑↓` 移动、`Space` 勾选、`Tab` 切换自由输入、`Enter` 下一题或提交、`Ctrl+P` 返回上一题、`Esc` 取消；长内容用 `PgUp/PgDn` 滚动。单选在按 Enter 时确认当前选项，多选需要显式勾选。headless 会明确取消提问，子任务的非交互运行会返回不可提问错误，不会默认为同意。

在 `~/.ccdp/config.json` 中设置：

```json
{
  "reasoning_effort": "high",
  "verbosity": "low"
}
```

也可用 `CCDP_REASONING_EFFORT` / `CCDP_VERBOSITY`，或启动参数 `ccdp --effort high --verbosity low`。会话空闲时 `/effort high`、`/verbosity low` 修改当前设置；不带参数时，`/effort` 打开带过渡动画的横向强度选择器（←/→ 预览、Enter 确认、Esc 取消），`/verbosity` 使用上下选择菜单；预览不改变底部的生效值。`/effort default`、`/verbosity default` 清除覆盖。设置随会话保存；原有冻结请求不受后续修改影响。`verbosity` 控制模型输出，与诊断日志 `--verbose` 不同。

`reasoning_effort` 接受 `none|minimal|low|medium|high|xhigh|max|ultra`，`verbosity` 接受 `low|medium|high`；未配置或空字符串时省略字段。实际支持的取值取决于所选模型和服务商，不支持时会保留 API 错误，不会静默重试并删除参数。Chat Completions 使用顶层 `reasoning_effort` / `verbosity`，格式见 [OpenAI 官方文档](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.2)。

DeepSeek 模型（名称以 `deepseek` 开头或使用官方端点）还会显式设置 `thinking.type`；`/effort none` 对应 `disabled`，其他显式档位对应 `enabled`。`reasoning_content` 随助手消息持久化，并在后续 DeepSeek 请求中回传，支持思考模式的连续工具调用；不会把该扩展字段发给其他模型。DeepSeek 的档位映射和限制见 [DeepSeek 官方文档](https://api-docs.deepseek.com/guides/thinking_mode/)。

## 构建

```bash
git clone https://github.com/cointem/ccdp.git
cd ccdp
go build -o ccdp ./cmd/ccdp
```

## 快速开始

```bash
# 1. 配置 API（base URL 和 model 可省略，使用内置默认值）
export CCDP_API_KEY="sk-..."
# export CCDP_BASE_URL="https://api.example.com/v1"
# export CCDP_MODEL="gpt-4.1"

# 2. 运行
./ccdp                    # 在当前目录打开 TUI
./ccdp -d ~/project       # 指定工作目录
./ccdp -m deepseek-chat   # 临时切换模型
```

直接运行 `ccdp` 即可开始使用，不需要初始化命令。需要多供应商、权限、
Hook、MCP 等高级配置时，再按下文创建可选的 `~/.ccdp/config.json`。

`/config` 查看脱敏后的有效配置及字段来源。显式环境变量和 CLI 值优先，`false`、`0`、空集合不会被误当作“未设置”。项目配置不能自行授权 hooks、MCP、自定义工具或放宽权限；审查项目配置后用 `/trust` 授权当前指纹，`/trust revoke` 撤销，配置变化需要重新授权。授权记录保存在项目之外。

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

欢迎区采用紧凑文字布局，为对话和输入留出空间；原 Gopher 素材保留在项目中。助手回复按 Markdown 排版，工具默认显示摘要，完整内容通过 `/transcript` 阅读。审批和提问在输入附近展示，保留最近的对话上下文。状态、选择器、附件和命令提示使用统一样式，适配深浅色与无色终端。

Gopher 字符画改编自 [Renée French 设计的 Go Gopher](https://go.dev/blog/gopher)，原形象采用 [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/) 许可。

常用斜杠命令（`/help` 查看全部）：

```
/model [name]   选择/切换模型      /mode [mode]      选择权限模式
/sandbox [m]    选择/切换沙箱      /compact          立即压缩上下文
/cost           token 用量与花费   /doctor           环境自检
/rewind [n]     回退历史          /fork [n]         分支新会话
/checkpoint     创建/恢复检查点    /diff /git        查看/执行 git
/sessions       会话列表          /resume <id>      恢复会话
/mcp /plugins   MCP 与插件状态     /skills           技能列表
/permissions    权限规则管理       /add-dir <dir>    扩展沙箱目录
/export [path]  导出 Markdown     /config set <k> <v>
/copy [id]      复制最近/指定回复   /trust [revoke]   授权/撤销项目配置
/agents        进入子 Agent 视图   /root /parent    返回主/父会话
/agent <id>    查看/定向控制子任务  /stop-tree       取消整棵运行树
/agent-history 分页查看保存的转录  /agent-output <item-id> [offset]
/transcript    阅读完整消息/工具    /details         同上
```

`/model`、`/mode`、`/sandbox` 不带参数会打开选择器；`Esc` 取消，不修改设置。长列表可继续向下导航，命令结果保留在转录中，不占用可被下一条命令覆盖的状态行。模型忙碌时切换显示为 pending，到安全边界才变为 active；安全策略修改需要空闲。

输入下方常驻 `deepseek-chat · high · 12.3k/200k`：当前生效模型、推理强度、当前上下文用量/容量。零用量也显示，未知容量显示 `—`；达到 80% 时保留警示颜色。这里的用量是当前上下文估算，不是会话累计消耗。窄屏先省略次要信息，必要时紧凑换行；模型、强度和用量不会因 `/statusline` 自定义而消失。

| 按键 | 作用 |
|------|------|
| `Enter` / `⌥Enter` | 发送 / 换行（`⌃J` 同效） |
| `⌃C` | 中断 agent；空闲时按两次退出 |
| `⌃O` | 打开 Agent 目录，查看子任务进度与审批；切换视图不停止任务 |
| `⌃V` / `⌃⌥V` | 粘贴剪贴板图片；没有图片时粘贴文本 |
| `Alt+←/→`、`Alt+Backspace` | 兼容附件快捷键；Mac 对应 `Option/⌥`，需终端将 Option 作为 Alt/Meta 发送 |
| 触控板 / 终端滚动快捷键 | 普通聊天的历史 scrollback，由终端处理 |
| `↑` / `↓` | 单行输入浏览历史，多行输入移动光标；选择器逐项导航 |
| `Home` / `End` | 移动输入光标；阅读器中跳到开头 / 结尾 |
| `PgUp` / `PgDn`、`⌃U` / `⌃D` | 滚动当前对话、选择器或阅读器 |
| 拖选后 `⌘C` | 终端原生复制；`/copy` 可直接复制回复原文 |
| `⌃P` / `⌃N` | 输入历史 |
| `Esc` | 关闭临时界面或返回，保留草稿 |
| `y` / `n` / `a` / `x` | 审批：允许 / 拒绝 / 会话总是允许 / 总是拒绝 |

阅读器中 `↑↓` / `PgUp/PgDn` 滚动内容，`←→` 切换消息，`/` 搜索、`n` 跳到下一处、`o` 切换原文、`c` 复制、`Esc` 返回。保存输出按页阅读，搜索与复制作用于当前页。长粘贴通过 `Ctrl+E` 展开/折叠，发送使用完整原文。阅读和子任务详情主动进入独立终端屏幕，返回后恢复主会话；导航不会清除主终端 scrollback。

不同终端的原生快捷键配置可能不同。自动化已覆盖布局和 PTY 控制序列，但尚未在 Ghostty 实测触控板、拖选与 `⌘C`；原因见集成验收记录。

子 Agent 支持同步等待和后台通知、独立会话续跑、输出分页及持久化结果去重。完整命令、运行生命周期、终端 scrollback 切换行为和安全边界见 [子 Agent 会话说明](docs/agent-sessions.md)。

支持截图粘贴、多图或纯图片消息；输入区显示附件编号、尺寸和大小。macOS 请使用 **Control+V**，不是 Command+V。Linux 需要 `wl-paste` 或 `xclip`，Windows/WSL 使用 PowerShell。图片只在粘贴时读取，并随会话保存；详细快捷键、限制和远程终端说明见 [图片粘贴](docs/image-paste.md)。

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
  protocol/        SessionClient 命令、回执、快照与订阅契约
  session/         事务 JSONL/内存 Store、typed facts、blob 与独占 writer
  commands/        help、补全、选择器及 busy/反馈元数据
  execution/       受控本地进程执行、输出限额与取消收束
  atomicfile/      原子文件发布与同步
  tui/             bubbletea 界面、斜杠命令、渲染与清洗
  tools/           内置工具（Bash/文件/搜索/git/进程/Task/Web…）
  llm/             OpenAI 兼容流式客户端
  mcp/             JSON-RPC over stdio/SSE 的 MCP 客户端
  permissions/     权限规则与审批判定
  sandbox/         沙箱（路径围栏 / sandbox-exec / ulimit）
  hooks/           shell hooks 执行器
  plugin/          插件 Host、ModelRegistry、GoHooks
  config/          配置加载与合并
  checkpoint/      文件检查点
  events/          进程内事件通知（不是权威会话存储）
```

## 安全说明

- API key 仅从环境变量 / config 运行时读取，不入库；`.gitignore` 已覆盖 `.ccdp/`、构建产物、`.env`
- 默认模式下所有写操作与非只读命令需人工审批；`always_deny` 优先级最高
- 进程、Git、hooks、stdio MCP 与命令 worker 复用受控执行边界；strict 使用 macOS `sandbox-exec`，缺失时报错而非降级。confine 仅提供受控文件路径围栏，不是任意 shell 的内核隔离
- 项目配置、信任记录与会话控制文件受写保护；只读 Git 查询禁用 external diff、textconv、pager 和 fsmonitor 等可执行助手
- headless 模式（`-p`）审批自动拒绝，适合 CI 只读场景
