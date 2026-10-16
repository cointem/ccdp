# ccdp

在终端里理解项目、修改代码、运行测试的 Go Coding Agent。

连接你选择的模型，用自然语言完成开发任务。ccdp 提供交互式终端界面、文件与命令工具、子 Agent 协作、代码审查和可恢复会话，也支持脚本调用。

**macOS · Go 1.27+ · 自备模型 API**

[快速开始](#快速开始) · [日常使用](#日常使用) · [模型配置](#模型配置) · [权限与沙箱](#权限与沙箱) · [脚本运行](#脚本运行) · [扩展](#扩展)

## 能做什么

- **在项目里工作**：读取和搜索文件、编辑代码、执行命令、运行后台进程；支持路径补全和图片输入。
- **先规划再执行**：进入 plan 模式梳理方案，确认计划后再修改项目。
- **委派子任务**：让子 Agent 处理独立任务，查看进度、补充指令和接收结果；支持隔离工作区与变更合并。
- **审查与交付**：审查未提交改动、暂存区、分支或 commit；准备提交、推送和 PR 的交付计划。
- **持续推进目标**：设置目标和 token 预算，在多轮执行中持续工作，直到完成、暂停、阻塞或达到限制。
- **保留工作上下文**：保存和恢复会话、自动压缩上下文、分支对话、创建文件检查点、导出转录。
- **接入自己的工具**：通过 MCP、Skills、自定义命令和 Hooks 扩展能力。

## 快速开始

需要 macOS、Go 1.27 或更新版本，以及支持流式输出和工具调用的模型 API。本地命令执行依赖 macOS Seatbelt；后端不可用时会拒绝执行。

### 1. 构建

```bash
git clone https://github.com/cointem/ccdp.git
cd ccdp
go build -o ccdp ./cmd/ccdp
```

### 2. 配置模型

创建 `~/.ccdp/config.json`。下面以 DeepSeek 为例：

```json
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
        }
      }
    }
  }
}
```

在启动 ccdp 的终端中设置密钥：

```bash
export DEEPSEEK_API_KEY="你的 API Key"
```

### 3. 开始工作

```bash
./ccdp                         # 在当前目录启动
./ccdp -d /path/to/project      # 在指定项目启动
```

进入后直接输入任务，例如：

```text
梳理这个项目的入口和主要模块。
修复登录接口的错误处理，并补充相关测试。
检查当前改动有没有遗漏的边界情况。
```

输入 `/help` 查看命令，`/model` 选择模型，`/config` 查看当前配置及其来源。

后文的 `ccdp` 假定可执行文件已加入 `PATH`；在构建目录中也可以使用 `./ccdp`。如需安装到 Go 的 bin 目录，运行 `go install ./cmd/ccdp` 并将该目录加入 `PATH`。

## 日常使用

### 常用命令

| 命令 | 用途 |
| --- | --- |
| `/model`、`/effort`、`/verbosity` | 选择模型、推理强度和回答详细程度 |
| `/plan`、`/mode` | 规划任务、选择审批模式 |
| `/context`、`/compact` | 查看上下文占用、立即压缩 |
| `/resume`、`/fork [n]` | 恢复会话、从历史消息分支 |
| `/rewind [n]`、`/checkpoint` | 回退对话历史、创建或恢复文件检查点 |
| `/review`、`/diff`、`/git` | 审查改动、查看差异和 Git 状态 |
| `/agents`、`/agent <id>`、`/pending` | 查看子任务、定向控制、处理审批与提问 |
| `/goal <目标>`、`/tasks` | 持续执行目标、查看工作清单 |
| `/stop-tree` | 停止整棵任务树 |
| `/transcript`、`/export [path]` | 阅读完整消息和工具输出、导出 Markdown |
| `/sandbox`、`/permissions`、`/trust` | 查看沙箱、管理权限、授权项目配置 |

运行中按 `Enter` 补充当前任务，按 `Tab` 将消息排到下一轮；命令或文件补全菜单打开时，`Tab` 优先补全。子任务审批会先提示，通过 `/pending` 或 `/agents` 进入处理。

`/rewind` 回退对话历史；文件恢复使用检查点和恢复流程。查看子 Agent 会话不会自动授予审批权限。

### 常用按键

| 按键 | 用途 |
| --- | --- |
| `Enter` | 发送；运行中补充当前任务 |
| `Tab` | 运行中将消息排到下一轮；补全菜单打开时优先补全 |
| `Option+Enter` / `Ctrl+J` | 换行 |
| `Ctrl+C` / `Esc` | 中断当前执行；空闲时连续两次 `Ctrl+C` 退出 |
| `Shift+Tab` | 切换审批模式：manual → edits → bypass |
| `Ctrl+O` | 打开完整转录阅读器 |
| `Ctrl+R` | 搜索输入历史 |
| `Ctrl+G` | 使用 `$EDITOR` 或 `$VISUAL` 编辑草稿 |
| `Ctrl+V` | 粘贴剪贴板图片；没有图片时粘贴文本 |
| 输入 `@` | 补全工作区路径 |

macOS 图片粘贴使用 **Control+V**。普通聊天的滚动和拖选复制由终端处理；阅读器与选择器支持 `PgUp` / `PgDn`。

项目约定可以写进 `AGENTS.md`。ccdp 也读取用户级 `~/.ccdp/AGENTS.md` 和项目级 `.ccdp/AGENTS.md`；`/init` 可创建项目指令文件。

## 模型配置

模型按 `provider/model` 选择，供应商的地址、密钥与协议放在同一条配置中。可以同时配置多个供应商，使用 `/model` 或 `ccdp -m <provider/model>` 切换。

| `wire_api` | 协议 |
| --- | --- |
| `chat` | OpenAI 兼容 Chat Completions |
| `responses` | OpenAI Responses |
| `anthropic` | Anthropic Messages |

OpenAI 兼容服务和本地模型服务可通过 `base_url` 接入。实际支持的工具调用、图片、推理参数和输出格式取决于模型与服务端。

- `api_key_env` 指定密钥环境变量；也可用 `api_key`，两者不能同时配置。
- `context_window` 和 `max_output_tokens` 按模型配置，建议填写服务端支持的值。省略时分别使用 200000 和 32000；输出上限必须小于上下文窗口。
- `fallback_model` 使用完整的 `provider/model`，在主模型失败时提供兜底。
- `reasoning_effort` 默认 `medium`，可用 `/effort` 或 `--effort` 调整；`verbosity` 未配置时省略，可用 `/verbosity` 或 `--verbosity` 设置。
- 设置变化影响后续请求；已经发出的请求使用自己的配置快照。不支持的参数会显示 API 错误。

<details>
<summary>常用运行参数配置</summary>

以下字段可加入上面的配置对象：

```json
{
  "reasoning_effort": "medium",
  "verbosity": "low",
  "permission_mode": "edits",
  "network_access": true,
  "max_parallel_tools": 4,
  "bash_timeout_seconds": 120,
  "max_turns": 20,
  "compact_threshold": 0.8,
  "keep_after_compact": 12,
  "additional_directories": ["/path/to/another/project"],
  "disallowed_directories": ["~/.ssh"],
  "always_allow": ["Bash:go test *"],
  "always_deny": ["Bash:git push*"],
  "enable_web_tools": true
}
```

`reasoning_effort` 接受 `none|minimal|low|medium|high|xhigh|max`；`verbosity` 接受 `low|medium|high`。模型是否支持这些档位由服务商决定。`max_turns: 0` 表示不限制每轮模型调用次数。

项目覆盖配置位于 `.ccdp/settings.json`，用 `/reload` 重新加载。CLI 和环境变量中的显式设置优先；用 `/config` 检查最终生效值。旧版顶层 `base_url` / `api_key` 和模型数组配置需要迁移到 `providers`。

</details>

## 权限与沙箱

审批模式决定哪些操作需要人工确认，默认是 **`edits`**。

| 模式 | 行为 |
| --- | --- |
| `manual` | 文件修改、非只读命令等需要审批 |
| `edits` | 自动接受文件编辑，非只读命令等仍需要审批 |
| `bypass` | 跳过普通审批；明确拒绝、plan 约束和沙箱限制仍生效 |

plan 是独立工作流：先以只读工具探索并提出计划，批准后执行。`always_deny` 优先于 `always_allow`。未经显式规则授权的原始 Bash Git 命令需要审批；`/git` 提供隔离执行的只读查询。

所有模型驱动的本地命令、后台进程、Hooks 和 stdio MCP 都经过 macOS Seatbelt。工作区默认可读写，工作区外的主机文件默认可读、不可写；`disallowed_directories` 可明确拒绝读写。审批命令不会自动扩大目录或网络授权。

`sessions` 目录默认可读，包括历史会话和工具保存的完整输出；它保留写保护，避免模型修改会话状态。用户显式配置的禁读目录仍然生效。代码审查复用普通子任务的工作区、工具和审批机制，审查目标及报告格式通过任务指令指定。

外网访问默认开启，可用 `network_access: false` 关闭。若使用 Clash 等本机代理的 Fake-IP DNS，在用户配置 `~/.ccdp/config.json` 中设置 `"network_proxy": "http://127.0.0.1:7897"`（替换为实际 HTTP 代理端口），或设置 `CCDP_NETWORK_PROXY`。此配置由 WebFetch/WebSearch 和命令执行共用；`/sandbox` 显示当前代理。项目设置不能改写可信代理。本机服务连接与监听需要单独授权；Web 工具和远程 MCP 还会检查网络能力。用 `/add-dir <dir>` 增加读写目录，`/add-dir --read-only <dir>` 指定只读目录，`/sandbox` 查看当前状态。

项目配置中的 Hooks、MCP、自定义工具和权限放宽需要用户通过 `/trust` 授权；配置变化后需重新授权，`/trust revoke` 可撤销信任。恢复会话不会重新授予临时 capability。

沙箱不能保证回收所有脱离托管进程组的后代。撤销授权时若无法确认旧策略进程已经退出，运行时会阻止继续派发，需要清理残留进程并新建会话。

## 持续目标

用 `/goal` 开始需要多轮推进的任务：

```text
/goal --budget 100000 完成登录模块并通过测试
/goal                         查看目标、状态和用量
/goal pause                   暂停目标与运行中的子任务
/goal resume                  继续执行
/goal edit <新目标>            修改目标，保留已用预算
/goal budget 200000            调整预算；off 移除预算
/goal clear                   停止并清除当前目标
```

目标在回答结束后继续推进，直到完成、阻塞、暂停或达到预算/轮次限制。同一会话只有一个当前目标；目标不会扩大权限或跳过审批。

预算计入非缓存输入与输出 token，包含关联子 Agent 和压缩调用；未报告用量时使用估算，在途请求可能使最终用量略超预算。恢复会话后需显式 `/goal resume`；`Esc` / `Ctrl+C` 中断时会暂停目标。

## 脚本运行

```bash
ccdp -p "解释这个项目的入口和主要模块" --max-turns 10
ccdp -p "检查当前改动" --output-format json
ccdp -p "检查当前改动" --output-format stream-json

ccdp --goal "完成登录模块并通过测试" --goal-budget 100000
ccdp -r <session-id> --resume-goal --output-format json

ccdp -c                       # 交互式选择历史会话
ccdp -r <session-id>           # 恢复指定会话
ccdp -l                       # 列出会话
ccdp --replay <session-id>     # 只读导出 Markdown 回放

ccdp review --staged           # 审查暂存区
ccdp review --base main        # 审查相对指定分支的改动
```

Headless 不能交互审批：需要审批的操作会被拒绝，提问会被取消。文件编辑仍遵守所选模式；运行测试或交付命令时，需要事先配置相应权限规则。

`--timeout` 设置 headless 超时秒数，`--max-turns` 限制模型调用次数。持续目标中调用次数累计，自动续跑不会重置；`--goal` 与 `-p`、流式输入、`-c` 互斥。

<details>
<summary>流式输入与持续目标退出码</summary>

用 `ccdp --input-format stream-json < input.ndjson` 接收流式输入。输出支持 `text`、`json` 和 `stream-json`；持续目标的 JSON 结果包含 `goal`，流式输出包含 `goal_changed` 和最终 `goal_stop`。

持续目标退出码：`0` 完成、`1` 执行错误、`2` 超时、`3` 阻塞、`4` 预算或轮次限制、`5` 暂停或清除、`130` 信号中断。`--resume-goal` 必须配合 `-r`，可用 `--goal-budget` 调整预算。

</details>

完整 CLI 参数见 `ccdp -h`。会话默认保存在 `~/.ccdp/sessions`；`--no-session-persistence` 可关闭持久化。恢复不会自动重跑结果未知的副作用。会话转录可能包含项目内容和工具输出，分享前应检查。

## 扩展

| 扩展方式 | 配置位置或入口 |
| --- | --- |
| 项目指令 | `AGENTS.md`、`.ccdp/AGENTS.md`、`~/.ccdp/AGENTS.md` |
| Skills | `.ccdp/skills/<name>/SKILL.md`、`~/.ccdp/skills/<name>/SKILL.md` |
| 自定义斜杠命令 | `.ccdp/commands/<name>.md`、`~/.ccdp/commands/<name>.md`，支持 `$ARGUMENTS` |
| MCP | 配置文件的 `mcp_servers`；支持 stdio、Streamable HTTP 和 SSE |
| Hooks | 配置文件的 `hooks`；支持工具调用、输入、压缩和会话生命周期事件 |
| 自定义 shell 工具 | 配置文件的 `tools`；JSON 参数从 stdin 输入，stdout 返回结果 |

项目级 Skills 和自定义命令优先于同名用户级内容。`/skills`、`/mcp` 和 `/plugins` 查看已加载扩展。Go 插件 API 支持注册工具、模型 Provider、Hooks 和会话回调。

<details>
<summary>MCP 和 Hooks 配置示例</summary>

以下字段可加入模型配置对象。stdio 示例中的程序需要预先安装；远程示例需要替换为实际服务器地址。

```json
{
  "mcp_servers": {
    "local": {
      "command": "/absolute/path/to/mcp-server",
      "args": []
    },
    "remote": {
      "transport": "sse",
      "base_url": "https://example.com/mcp",
      "network_authorized": true
    }
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Write",
        "hooks": [{ "command": "./guard.sh", "timeout": 30 }]
      }
    ]
  }
}
```

HTTP 连接优先使用 Streamable HTTP，初始化返回 404/405 时回退旧 SSE。`network_authorized` 授权连接服务器，每次工具操作仍受权限和网络能力检查。MCP 支持工具目录热刷新与断连清理。

Hooks 可通过 `{"decision":"ask"}` 请求审批、退出码 `2` 阻断操作、`{"continue":false}` 停止会话循环。

</details>

## 开发

LLM 协议使用 Go HTTP 流式实现；终端界面基于 Bubble Tea，MCP 使用官方 Go SDK。会话通过单一状态循环管理，以事务 JSONL 日志持久化。

```bash
go build ./cmd/ccdp
go test ./...
go test -race ./... -timeout=300s
go vet ./...
```

完整测试需要可用的 macOS Seatbelt 和本地 HTTP 监听能力。

| 目录 | 职责 |
| --- | --- |
| `cmd/ccdp` | 启动、CLI、headless 和退出码 |
| `internal/agent` | 会话状态、回合编排、目标和业务流程 |
| `internal/session` | 日志、附件、回放、归档和提交 |
| `internal/protocol`、`internal/sessionview` | 命令、回执、快照与界面视图 |
| `internal/tooling`、`internal/tools` | 工具调度、准入和具体工具 |
| `internal/supervision` | 子任务目录、容量和生命周期 |
| `internal/execution`、`internal/fsops`、`internal/changes` | 进程执行、文件操作和变更恢复 |
| `internal/permissions`、`internal/sandbox` | 权限判定与 macOS 沙箱 |
| `internal/llm`、`internal/mcp` | 模型协议和 MCP 连接 |
| `internal/tui` | 终端交互与渲染 |

反馈问题请使用 [GitHub Issues](https://github.com/cointem/ccdp/issues)，附上运行命令、错误信息和脱敏后的配置。

搜索工具 `Rg` 直接调用系统安装的 `rg`（ripgrep），普通搜索、多行搜索和代码审查都使用同一原生引擎。支持 rg 的 Unicode 正则语义、固定字符串、大小写选项和结果分页；搜索经过只读 Seatbelt，遵循当前会话的读取策略。不再提供 `Grep` 或 Go 搜索后备实现；缺少 `rg` 时明确报错，macOS 可用 `brew install ripgrep` 安装。
