# ccdp

一个用 Go 编写的终端 Coding Agent：**Codex 式事件驱动循环 + Claude Code 式交互体验**。通过任意 OpenAI 兼容协议（`base_url` + `api_key` + `model`）驱动，自带 TUI、权限门禁、沙箱、MCP、会话持久化与上下文压缩。

```
ccdp 0.2.0 · Go 1.27+ · 无第三方 LLM SDK 依赖（纯 HTTP 流式实现）
```

## 功能特性

- **Agent 循环** — Infer → ToolDispatch → ApprovalGate → Compact，支持并行工具执行（`max_parallel_tools`）、工具结果聚合预算、截断回复自动续写、`--max-turns` / `--max-budget` 双重熔断
- **实时 TUI**（Bubble Tea）— 紧凑文字欢迎区、适配深浅终端的青蓝主题、动态工作指示与每轮耗时、紧凑工具结果；完成后的耗时随回复保留在转录中；默认 inline 转录、共享可滚动选择器、状态栏、多行输入；普通聊天不捕获鼠标，历史滚动与拖选交给终端
- **需求澄清** — `AskUserQuestion` 支持 1–4 题、单选/多选和自由补充；plan 模式可用，答案不代替执行审批
- **推理与输出控制** — `reasoning_effort` / `verbosity` 支持配置、环境变量、CLI 和 `/effort` / `/verbosity`；会话保存与恢复
- **权限体系** — `manual` / `edits`（默认）/ `bypass` 审批策略与独立 plan 工作流；兼容 `/mode plan`，always_allow / always_deny 规则（glob）、会话级审批记忆、turn 级审批缓存
- **沙箱** — macOS 上所有模型驱动的本地命令与外部程序固定进入 Seatbelt；后端不可用时拒绝执行。通过具体现授权目录和网络能力调整访问范围，`/sandbox` 显示运行诊断
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

也可用 `CCDP_REASONING_EFFORT` / `CCDP_VERBOSITY`，或启动参数 `ccdp --effort high --verbosity low`。会话空闲时 `/effort high`、`/verbosity low` 修改当前设置；不带参数时，`/effort` 打开带过渡动画的横向强度选择器（←/→ 预览、Enter 确认、Esc 取消），`/verbosity` 使用上下选择菜单；预览不改变底部的生效值。`/verbosity default` 清除覆盖，回到未配置状态。设置随会话保存；原有冻结请求不受后续修改影响。`verbosity` 控制模型输出，与诊断日志 `--verbose` 不同。

`reasoning_effort` 接受 `none|minimal|low|medium|high|xhigh|max`，`verbosity` 接受 `low|medium|high`；`reasoning_effort` 未配置时按内置默认 **`medium`** 发送，`verbosity` 未配置时省略字段。实际支持的取值取决于所选模型和服务商，不支持时会保留 API 错误，不会静默重试并删除参数。Chat Completions 使用顶层 `reasoning_effort` / `verbosity`，格式见 [OpenAI 官方文档](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.2)。Anthropic 线路使用新版 adaptive thinking：非 `none` 档位发送 `thinking: {type:"adaptive"}` 并在 `output_config.effort` 携带 `low|medium|high|max`（`minimal→low`、`xhigh→high`），`none` 关闭思考，旧的 `budget_tokens` 形态不再发送。

DeepSeek 模型（名称以 `deepseek` 开头或使用官方端点）还会显式设置 `thinking.type`；`/effort none` 对应 `disabled`，其他显式档位对应 `enabled`。`reasoning_content` 随助手消息持久化，并在后续 DeepSeek 请求中回传，支持思考模式的连续工具调用；不会把该扩展字段发给其他模型。DeepSeek 的档位映射和限制见 [DeepSeek 官方文档](https://api-docs.deepseek.com/guides/thinking_mode/)。

## 构建

```bash
git clone https://github.com/cointem/ccdp.git
cd ccdp
go build -o ccdp ./cmd/ccdp
```

## 快速开始

```bash
# 1. 按下方示例创建 ~/.ccdp/config.json，再设置对应密钥
export DEEPSEEK_API_KEY="sk-..."

# 2. 运行
./ccdp                    # 在当前目录打开 TUI
./ccdp -d ~/project       # 指定工作目录
./ccdp -m deepseek/deepseek-chat   # 临时切换模型
```

模型与供应商统一配置在 `~/.ccdp/config.json`，选择模型使用 `provider/model`。旧版顶层连接配置与模型数组不再支持，启动时会报告迁移错误。

`/config` 查看脱敏后的有效配置及字段来源。显式环境变量和 CLI 值优先，`false`、`0`、空集合不会被误当作“未设置”。项目配置不能自行授权 hooks、MCP、自定义工具或放宽权限；审查项目配置后用 `/trust` 授权当前指纹，`/trust revoke` 撤销，配置变化需要重新授权。授权记录保存在项目之外。

最小配置：

```json
{
  "model": "deepseek/deepseek-chat",
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com/v1",
      "api_key_env": "DEEPSEEK_API_KEY",
      "wire_api": "chat",
      "models": {
        "deepseek-chat": { "context_window": 128000 }
      }
    }
  }
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
ccdp --mode edits               # manual | edits | bypass
ccdp --fallback-model gpt-4o     # 主模型失败时的兜底
ccdp --add-dir /data             # 额外授权沙箱目录
ccdp --allowedTools Bash,Read    # 跳过审批门的工具
```

其余 flags：`--system-prompt[-file]`、`--append-system-prompt`、`--disallowedTools`、`--session-id`、`--no-session-persistence`、`--timeout`（headless 超时秒数）、`--verbose` / `--debug`、`--config-file`。`ccdp --version` 查看版本。

## 配置参考（`~/.ccdp/config.json`）

模型名只在 `providers.<id>.models` 对象里声明一次，模型选项跟随该记录。`model` 和 `fallback_model` 使用完整的 `provider/model` 引用；同名模型可出现在不同供应商下，API 请求只发送模型部分。`api_key` 和 `api_key_env` 二选一，供应商间不继承密钥。`wire_api` 只能是 `chat` 或 `responses`，拼错会报错。

`context_window` 省略时暂用内置 200000；自定义模型建议明确填写，数值必须与服务端实际支持的窗口一致。`max_output_tokens` 是每个模型的单次生成上限，思考与正文共用；省略或填 `0` 时默认 **32000**，必须小于该模型的上下文窗口。切换模型、fallback 和子 agent 都使用目标模型自己的值。旧的全局 `max_reply_tokens` / `CCDP_MAX_REPLY_TOKENS` 不再接受。推理强度是单一全局设置（`reasoning_effort`），未配置时内置默认 **`medium`**；不提供按模型默认值，旧的 `models.<model>.reasoning_effort` 不再接受。

```jsonc
{
  "model": "deepseek/deepseek-chat",
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com/v1",
      "api_key_env": "DEEPSEEK_API_KEY",
      "wire_api": "chat",
      "models": {
        "deepseek-chat": {
          "context_window": 128000,
          "max_output_tokens": 8192
        },
        "deepseek-reasoner": {
          "context_window": 128000
          // 可选 pricing: { "input_per_million": 实际单价, "output_per_million": 实际单价 }
        }
      }
    }
  },

  "permission_mode": "edits",
  "always_allow": ["Bash:git status*"],
  "always_deny":  ["Bash:git push*", "Write:/etc/**"],

  // 上下文管理
  "compact_threshold": 0.8,        // 达到窗口比例自动压缩
  "keep_after_compact": 12,        // 压缩后保留最近消息数
  "max_tool_output_chars_per_turn": 200000,

  "sandbox_limits": { "cpu_seconds": 600, "memory_mb": 2048 },
  "network_access": false,          // true 授予本地沙箱进程外联能力
  "additional_directories": ["/data"],
  "additional_read_only_directories": ["/reference"],
  "disallowed_directories": ["~/.ssh"],
  "max_parallel_tools": 4,         // 0 = 顺序执行
  "bash_timeout_seconds": 120,

  "fallback_model": "deepseek/deepseek-reasoner",
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
    "remote": { "transport": "sse", "base_url": "http://localhost:8080/sse", "network_authorized": true }
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

项目级覆盖（`.ccdp/settings.json`，`/reload` 热加载）：`permission_mode`、`always_allow` / `always_deny`、`hooks`、`enable_web_tools`。项目设置不能扩大沙箱授权；其中 `network_access: false` 可收紧外联能力，`disallowed_directories` 只能增加拒绝目录。额外授权目录和外联能力由用户配置或本会话的明确 capability approval 授予。

## macOS Seatbelt 沙箱

沙箱的最终目标边界与验收条件见 [macOS 沙箱设计](docs/macos-sandbox.md)。

所有模型驱动的本地命令、后台进程、hooks、stdio MCP 和搜索子进程始终由 macOS Seatbelt 执行。Seatbelt 或策略加载失败时，执行入口会报错并停止；CCDP 不自动移除限制重试。非 macOS 平台不提供本地执行回退。没有 confine/strict/none 沙箱档位，也没有旧档位字段或兼容映射。

workspace 默认可读写，工作区外的主机文件默认可读、不可写。`additional_directories` 授予额外读写访问；`additional_read_only_directories` 可在可写范围内划出禁止写入的目录，位于可写范围外时不改变默认读取行为；`disallowed_directories` 始终拒绝读写并优先于允许目录。Git config、hooks 和 worktree 指针受保护，普通提交仍可运行。`network_access` 明确控制本地子进程的 TCP/UDP 外联；它不自动开放本机服务连接或监听，后者按具体协议和端口授权。Seatbelt 的 `localhost:<port>` 规则限制端口，但不能保证只匹配字面上的 `127.0.0.1` 或单一 loopback 接口。审批通过命令不会扩大这些授权。Web 工具与远程 MCP 使用主进程网络，另受网络 capability 检查；远程 MCP server 连接还需在服务器配置中由用户明确设置 `network_authorized: true`，这项连接授权不代替每次工具操作的权限审批或网络 capability。内置文件工具仍由主进程的路径策略检查。`/sandbox` 显示 Seatbelt、workspace、授权目录、网络和运行时状态；它显示可能残留旧策略后代的 sticky 风险标志，不提供准确 PID 清单。

在 TUI 中，`/sandbox` 只读查看诊断，`/sandbox revoke all` 请求撤销本会话临时 capability 并收束托管的进程、MCP 和当前工作；`/add-dir <dir>` 授予额外读写目录，`/add-dir --read-only <dir>` 授予只读目录，`/disallowed-dir <dir>` 添加拒绝目录。用 `/config set network-access allow|deny` 更新持久子进程网络授权。`once` 与 `session` capability 只保存在运行时，不会从恢复会话的记录中重新授权。

本地子进程可能通过 `setsid` 留下脱离进程组的后代，因此其生命周期 witness 在本会话内保持 sticky。该事实本身不阻止正常派发或新增授权；用户请求收紧或撤销且系统无法确认旧策略后代已经退出时，会进入 `revocationPending`，继续阻断派发和设置，不能通过再授权绕过。重试不会清除此状态；需独立清理可能残留的进程并新建会话。普通关闭会清理受管理资源并报告不确定性，不代表可热撤销所有运行进程的策略。

截至 2026-09-26，本机真实 Seatbelt 集成下的 `go test ./...`、agent/tools/mcp/netguard 全包 race、`go vet ./...` 及最终 revision/ABA 修复后的授权/reload/approval 定向 race 均通过。检查覆盖 loopback 默认拒绝、外联授权不解锁 loopback、按端口区分的 TCP connect/listen、Unix socket 拒绝、公网 TCP dial，以及生产默认路径发现的 Go、Node、Python toolchain。它们是本机验收结果，不是通用安全证明。sticky witness 仍无法证明经 `setsid` 脱离进程组的后代已退出；撤销/收紧无法确认时会持续阻断派发和设置，必须独立清理可能残留的进程并新建会话。

## 权限模式

| 模式 | 行为 |
|------|------|
| `manual` | 写文件、非只读 Bash 等需要审批（y/n，`a` 本会话总是允许） |
| `edits`（默认） | 自动接受文件编辑，Bash 等仍需审批 |
| `plan` | 只读模式：模型只能产出计划，批准后执行 |
| `bypass` | 全部放行（风险自负） |

规则匹配支持 bare 命令（`"go test *"`）与工具前缀 glob（`"Bash:git status*"`、`"Write:/tmp/*.log"`）；含 `/` 的路径规则中 `*` 不跨越目录分隔符、`**` 跨越。deny 永远优先于 allow。旧名称 `default` / `acceptEdits` / `bypassPermissions` 仍被接受，内部按上表语义映射。

## TUI 命令与按键

欢迎区采用紧凑文字布局，为对话和输入留出空间；原 Gopher 素材保留在项目中。助手回复按 Markdown 排版，工具默认显示摘要，完整内容通过 `/transcript` 阅读。审批和提问在输入附近展示，保留最近的对话上下文。状态、选择器、附件和命令提示使用统一样式，适配深浅色与无色终端。


常用斜杠命令（`/help` 查看全部）：

```
/model [name]   选择/切换模型      /mode [mode]      选择权限模式
/sandbox [revoke all] Seatbelt 诊断/撤销临时授权   /compact 立即压缩上下文
/cost           token 用量与花费   /context         上下文占用可视化
/rewind [n]     回退历史          /fork [n]         分支新会话
/checkpoint     创建/恢复检查点    /diff /git        查看/执行 git
/resume [id]    恢复会话（无参数进入选择器）
/mcp /plugins   MCP 与插件状态     /skills           技能列表
/permissions    权限规则管理       /add-dir <dir>    授予读写目录
/export [path]  导出 Markdown     /config set <k> <v>
/trust [revoke] 授权/撤销项目配置
/agents        进入子 Agent 视图   /root /parent    返回主/父会话
/agent <id>    查看/定向控制子任务  /stop-tree       取消整棵运行树
/agent-history 分页查看保存的转录  /agent-output <item-id> [offset]
/transcript    阅读完整消息/工具    /details         同上
/pending       打开待处理审批/提问 /next <text>     排队到下一轮
/reconnect     重新连接会话更新    /tasks           切换任务清单
```

`/model`、`/mode` 不带参数会打开选择器；`/sandbox` 只读显示 Seatbelt 与授权诊断，`/sandbox revoke all` 请求撤销本会话临时授权。长列表可继续向下导航，命令结果保留在转录中，不占用可被下一条命令覆盖的状态行。设置类命令（`/model`、`/mode`、`/permissions`、`/plan`、`/effort`、`/verbosity` 与 `Shift+Tab`）无论空闲还是忙碌都立即生效：每次 provider 请求已在 step 边界冻结自身配置与绑定、每次授权也在锁内原子读取模式，故中途切换不会扰动进行中的请求或已做出的判定，只影响下一次调用，对齐 codex/claude-code。

输入下方常驻 `deepseek-chat · high · 12.3k/200k`：当前生效模型、推理强度、当前上下文用量/容量。零用量也显示，未知容量显示 `—`；达到 80% 时保留警示颜色。这里的用量是当前上下文估算，不是会话累计消耗。窄屏先省略次要信息，必要时紧凑换行；模型、强度和用量不会因 `/statusline` 自定义而消失。

| 按键 | 作用 |
|------|------|
| `Enter` / `⌥Enter` | 发送 / 换行（`⌃J` 同效） |
| `⌃C` | 中断 agent；空闲时按两次退出 |
| `⌃O` | 在独立阅读器中展开完整转录与工具输出（详细模式） |
| `⌃T` | 显示/隐藏工作任务清单（`✔` 完成 · `●` 进行中 · `○` 待办） |
| `Shift+Tab` | 就地循环权限模式（manual → edits → bypass）；忙碌时也立即生效，只影响下一次授权判定 |
| `⌃←` / `⌃→` | 在同级子 Agent 之间快速切换；Agent 目录见 `/agents` |
| `⌃V` / `⌃⌥V` | 粘贴剪贴板图片；没有图片时粘贴文本 |
| `Alt+←/→`、`Alt+Backspace` | 兼容附件快捷键；Mac 对应 `Option/⌥`，需终端将 Option 作为 Alt/Meta 发送 |
| 触控板 / 终端滚动快捷键 | 普通聊天的历史 scrollback，由终端处理 |
| `↑` / `↓` | 单行输入浏览历史，多行输入移动光标；选择器逐项导航 |
| `Home` / `End` | 移动输入光标；阅读器中跳到开头 / 结尾 |
| `PgUp` / `PgDn`、`⌃U` / `⌃D` | 滚动当前对话、选择器或阅读器 |
| 拖选后 `⌘C` | 终端原生复制；阅读器中 `c` 可复制原文 |
| `⌃P` / `⌃N` | 输入历史 |
| `⌃R` | 增量反向搜索输入历史；`⌃R`/`↑` 翻下一处，`Enter` 回填到输入框，`Esc` 取消 |
| `⌃G` | 用 `$EDITOR`（或 `$VISUAL`）编辑当前草稿，挂起/恢复终端 |
| 输入 `@` | 触发工作区路径补全；`↑↓` 选择，`Tab`/`Enter` 插入，目录可继续下钻 |
| `Esc` | 关闭临时界面或返回，保留草稿；普通运行视图中中断当前执行 |
| 审批中的 `↑↓` / `Enter` | 先明确选择，再确认；默认不选中允许 |
| 审批中的 `Esc` / `⌃O` | 稍后处理（不拒绝） / 展开完整审批内容 |

底部交互区一次只显示一个审批、提问或选择器，草稿在关闭后恢复。后台子 Agent 的审批只显示提醒，通过 `/pending` 或 `/agents` 进入处理，不抢输入焦点。运行时普通输入用于 steer，`/next <text>` 排队到下一轮；退出时若仍有运行任务，需要明确确认。断线最多自动重连 5 次，也可用 `/reconnect` 手动重连；恢复只重新订阅状态，不自动重发命令，未提交草稿保留。

阅读器中 `↑↓` / `PgUp/PgDn` 滚动内容，`←→` 切换消息，`/` 搜索、`n` 跳到下一处、`o` 切换原文、`c` 复制、`Esc` 返回。保存输出按页阅读，搜索与复制作用于当前页。长粘贴通过 `Ctrl+E` 展开/折叠，发送使用完整原文。阅读和子任务详情主动进入独立终端屏幕，返回后恢复主会话；导航不会清除主终端 scrollback。

不同终端的原生快捷键配置可能不同。自动化已覆盖布局和 PTY 控制序列，但尚未在 Ghostty 实测触控板、拖选与 `⌘C`。

子 Agent 支持同步等待和后台通知、独立会话续跑、输出分页及持久化结果去重；命令行为可通过 `/help` 与 `/agents` 视图查看。

支持截图粘贴、多图或纯图片消息；输入区显示附件编号、尺寸和大小。macOS 请使用 **Control+V**，不是 Command+V。Linux 需要 `wl-paste` 或 `xclip`，Windows/WSL 使用 PowerShell；图片只在粘贴时读取，并随会话保存。

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
  sandbox/         macOS Seatbelt profile 与路径策略
  hooks/           shell hooks 执行器
  plugin/          插件 Host、ModelRegistry、GoHooks
  config/          配置加载与合并
  checkpoint/      文件检查点
  events/          进程内事件通知（不是权威会话存储）
```

## 安全说明

- API key 仅从环境变量 / config 运行时读取，不入库；`.gitignore` 已覆盖 `.ccdp/`、构建产物、`.env`
- 默认模式下所有写操作与非只读命令需人工审批；`always_deny` 优先级最高
- 进程、Git、hooks、stdio MCP 与命令 worker 复用受控执行边界；模型驱动的本地执行始终使用 macOS Seatbelt，后端缺失或策略加载失败时停止。无隔离操作需由用户在外部终端执行
- 项目配置、信任记录与会话控制文件受写保护；只读 Git 查询禁用 external diff、textconv、pager 和 fsmonitor 等可执行助手
- headless 模式（`-p`）审批自动拒绝，适合 CI 只读场景

权限模式通过 `/mode` 或 Shift+Tab 切换后按项目保存。同一 Git 仓库的子目录共用设置，无 Git 时按启动目录区分；记录位于用户配置旁的 `project-permissions/`（默认 `~/.ccdp/project-permissions/`），不会写入仓库。CLI / 环境变量显式指定优先。新建子 agent 继承主 agent 的权限；`edits` 自动允许文件写入，普通命令审批与硬性限制保持生效。
