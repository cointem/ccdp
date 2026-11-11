# 使用指南

[返回首页](../README.md)

安装和首次连接模型见 [README](../README.md#开始使用)。这里说明会话中的常用操作、运行设置和扩展方式。

[日常使用](#日常使用) · [模型配置](#模型配置) · [权限与沙箱](#权限与沙箱) · [记忆](#记忆) · [持续目标](#持续目标) · [脚本运行](#脚本运行) · [桌面操作](#桌面操作) · [扩展](#扩展)

## 日常使用

### 常用命令

| 命令 | 用途 |
| --- | --- |
| `/model`、`/effort`、`/verbosity` | 选择模型、推理强度和回答详细程度 |
| `/mode` | 选择 `auto` 或 `manual` 审批模式 |
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

以 `/` 开头但未匹配到命令的输入（例如 `//` 代码注释、`/tmp/example.go` 路径），以及无法解析的命令输入，会原样作为普通消息发送，保留缩进和换行。

`/rewind` 回退对话历史；文件恢复使用检查点和恢复流程。查看子 Agent 会话不会自动授予审批权限。

工作清单随会话保存和恢复，保留任务顺序、状态和优先级。更新未保存成功时，仍显示上一次已保存的清单。

### 常用按键

| 按键 | 用途 |
| --- | --- |
| `Enter` | 发送；运行中补充当前任务 |
| `Tab` | 运行中将消息排到下一轮；补全菜单打开时优先补全 |
| `Option+Enter` / `Ctrl+J` | 换行 |
| `Esc` 连按两次 | 500 毫秒内清空当前草稿（文字和图片），不打断任务 |
| `Ctrl+C` / 空输入时 `Esc` | 中断当前执行；空闲时连续两次 `Ctrl+C` 退出 |
| `Shift+Tab` | 切换审批模式：auto ↔ manual |
| `Ctrl+O` | 打开完整转录阅读器 |
| `Ctrl+R` | 搜索输入历史 |
| `Ctrl+G` | 使用 `$EDITOR` 或 `$VISUAL` 编辑草稿 |
| `Ctrl+V` | 粘贴剪贴板图片；没有图片时粘贴文本 |
| 输入 `@` | 补全工作区路径 |

macOS 图片粘贴使用 **Control+V**。普通聊天的滚轮滚动由 TUI 处理，拖选文字后松开鼠标会复制选区；使用终端原生选择需按终端的鼠标覆盖键，通常为 `Shift` 拖选。阅读器与选择器支持 `PgUp` / `PgDn`。

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

以下运行参数可加入 [README 中的模型配置](../README.md#开始使用)的顶层对象：

```json
{
  "reasoning_effort": "medium",
  "verbosity": "low",
  "permission_mode": "auto",
  "local_development_network": true,
  "network_access": true,
  "max_parallel_tools": 4,
  "max_concurrent_agents": 4,
  "enable_project_memory": false,
  "memory_read_only": false,
  "memory_history": false,
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

项目覆盖配置位于 `.ccdp/settings.json`，用 `/reload` 重新加载。CLI 和环境变量中的显式设置优先；用 `/config` 检查最终生效值。

`max_parallel_tools` 控制同一请求中的工具并发，`max_concurrent_agents` 控制运行中的普通子任务；默认均为 4，范围均为 1–16。修改会影响后续调度，不会中止已经运行的任务。

`bash_timeout_seconds` 设置前台命令的默认超时，后台命令默认不设执行期限。工具输出预算使用字节：`max_result_size_bytes` 限制单个结果，`max_tool_output_bytes_per_turn` 限制整轮结果。

长输出通过 `ReadOutput(output_id, offset, limit)` 按字节续读，包括 WebFetch 全文和已结束命令的保留日志。停止进程或释放临时日志后，已保存的输出仍可读取；运行中的命令通过 `Process` 查看进度或发送输入。

</details>

## 权限与沙箱

启动时直接使用当前配置与项目已保存的权限模式，默认 **`auto`**。可用 `--mode manual` 显式选择逐次审批；审批模式与网络配置分别生效。恢复会话使用当前配置与有效的已保存限制。

| 模式 | 行为 |
| --- | --- |
| `auto` | 已授权环境内的文件修改、构建、测试、Shell 和进程输入直接执行；新增访问范围或未知工具等例外才请求决定 |
| `manual` | 文件修改、非只读命令等需要审批 |

需要先规划时，直接在消息中要求先给方案即可。`always_deny` 优先于 `always_allow`；两种模式都遵守明确拒绝、只读子任务和沙箱边界。`Process.read`、`Process.wait`、`Process.stop` 只操作当前会话的已有句柄，无需新审批。`/git` 提供隔离执行的只读查询。

同一命令及其后代进程可以相互发送信号，支持 `kill`、`kill(pid, 0)`、`timeout` 和测试框架回收自己启动的 daemon。跨命令管理后台服务时，用 `Bash(background=true)` 运行服务的前台模式，通过返回的句柄调用 `Process.stop`；任意宿主进程不在这个授权范围内。

工作区默认可读写，外部主机文件默认只读；禁读目录、常见凭证目录、provider 配置和其他会话数据受保护。本地操作在独立执行环境中运行，不会自动退回无沙箱执行。

外网访问默认开启，可用 `network_access: false` 关闭。`local_development_network` 默认开启，允许本机 TCP/UDP 通信和动态端口监听；关闭后仍可按方向、协议和端口申请单个本机服务。若使用本机 HTTP 代理，在用户配置中设置 `network_proxy`（例如 `http://127.0.0.1:7897`）或 `CCDP_NETWORK_PROXY`；项目配置只能收紧网络能力。

工作区、额外可写目录、当前会话临时目录和项目缓存内默认允许 Unix stream/datagram socket 的监听和连接，不依赖外网开关。只读、禁读、受保护目录和其他会话仍不可通过 socket 访问。临时 IPC 使用 `$TMPDIR` 或 `$XDG_RUNTIME_DIR`，二者指向短路径的私有目录，避免 macOS Unix socket 路径长度限制；直接写公共 `/tmp` 不在默认授权内。用 `/add-dir <dir>` 增加读写及目录内 Unix socket 权限，`/add-dir --read-only <dir>` 指定只读目录，`/sandbox` 查看环境状态。

开发进程可以创建自己的伪终端，Python `multiprocessing` 的随机命名信号量可用；匿名 socket、文件锁和文件映射共享内存也可正常使用。macOS 的 setuid `/bin/ps` 在 Seatbelt 下仍无法执行，可用已有 `Process` 句柄管理命令，或用 `lsof` 检查文件和端口。

额外访问按资源合并等待，批准后继续原调用；父会话与普通子任务共享资源决定，once 授权不扩散。相同拒绝不再重复审核或弹窗，连续三轮重复拒绝会结束无效工具循环。可选 guardian 只审例外，格式错误、额度耗尽或不可用都不会自动放行。

项目配置中的 Hooks、MCP、自定义工具和权限放宽需要用户通过 `/trust` 授权；配置变化后需重新授权，`/trust revoke` 可撤销信任。恢复会话不会重新授予临时 capability。

撤权或执行器丢失后若清理不确定，会记录受影响的可写目录并暂停该环境，重启不能清除记录；聊天、诊断、已有结果和进程清理仍可用。不共享受影响目录的工作区可以继续，并使用新的临时目录。结果不明的调用不重跑，可用 `/sandbox` 查看当前环境状态。

## 记忆

记忆默认关闭，可用 `/memory on` 开启。开启后用于保存长期偏好、用户纠正和经过验证的经验，可以直接说“记住……”“把……改成……”“忘记……”。任务进度、猜测和密钥不应写入长期记忆。

默认写入项目范围，明确通用于所有项目的偏好才写用户范围。同一 Git 仓库的 worktree 共享项目记忆；子 Agent 通过父任务获得相关记忆，由父 Agent 检查后保存新经验。

- `/memory` 查看状态、目录和记忆列表；`/memory read <id>` 查看正文与来源。
- `/memory search <关键词>` 搜索项目记忆；模型也可指定用户范围搜索。
- `/memory save <内容>` 手动保存；`/memory save --user <内容>` 保存通用偏好。不需要标题、摘要或版本号。
- `/memory edit <id> <原文> | <新文>` 修改唯一匹配的片段；`/memory delete <id>` 忘记一条记忆。用户范围使用 `user:<id>`。
- `/memory tidy` 合并完全重复的项目笔记并刷新索引；语义合并由模型读取后编辑，避免盲目覆盖。
- `/memory on` 自动读写，`/memory readonly` 只读取，`/memory off` 不读取或生成。设置随会话保存，也可通过 `enable_project_memory`、`memory_read_only` 配置默认值。`--no-session-persistence` 下只读。
- `/memory history on` 开启当前会话的可选历史提取，`off` 关闭；配置项为 `memory_history`，默认关闭。开启后后台在空闲至少一分钟时检查，检查间隔至少五分钟，分批处理已完成且未处理的回合；恢复会话后继续处理进度。需要调用模型，但不阻塞正常聊天。
- `/memory extract [session-id]` 手动触发一批历史提取，省略 ID 时使用当前会话。只接收同一项目的根会话，跳过关闭记忆、只读、未完成、撤回和过大的回合；自动提取还跳过当时未开启历史提取的回合。

正文是 Markdown 文件，可通过 `/memory` 查看实际目录。默认根目录为 `~/.ccdp/projects`，可以用 `project_memory_dir` 调整。每个范围最多 64 条有效记忆、每条 8 KiB、合计 256 KiB；读取失败不会阻止聊天，保存失败会明确报错。

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

Headless 按当前权限配置直接启动，无需额外确认。它不能交互审批：需要审批的操作会被拒绝，提问会被取消。

`--timeout` 设置 headless 超时秒数，`--max-turns` 限制模型调用次数。持续目标中调用次数累计，自动续跑不会重置；`--goal` 与 `-p`、流式输入、`-c` 互斥。

<details>
<summary>流式输入与持续目标退出码</summary>

用 `ccdp --input-format stream-json < input.ndjson` 接收流式输入。输出支持 `text`、`json` 和 `stream-json`；持续目标的 JSON 结果包含 `goal`，流式输出包含 `goal_changed` 和最终 `goal_stop`。

持续目标退出码：`0` 完成、`1` 执行错误、`2` 超时、`3` 阻塞、`4` 预算或轮次限制、`5` 暂停或清除、`130` 信号中断。`--resume-goal` 必须配合 `-r`，可用 `--goal-budget` 调整预算。

</details>

完整 CLI 参数见 `ccdp -h`。会话默认保存在 `~/.ccdp/sessions`；`--no-session-persistence` 可关闭持久化。恢复不会自动重跑结果未知的副作用。会话转录可能包含项目内容和工具输出，分享前应检查。

## 桌面操作

Computer 使用系统原生接口，需要 macOS 14+ 和 Xcode Command Line Tools。在源码目录中启用 CGO 构建后运行：

```bash
CGO_ENABLED=1 go install ./cmd/ccdp
ccdp --computer-use
```

首次启动按系统提示，在「系统设置 → 隐私与安全性」中给启动 ccdp 的应用（例如 Terminal 或 iTerm）开启「屏幕录制」和「辅助功能」，然后重新启动。如果系统列出 ccdp 本身，则给该项授权。缺少权限时启动会直接说明原因，模型不会进入重复请求循环。开启后可以输入「打开浏览器，搜索……」等桌面任务；模型必须支持图片输入。

`auto` 模式在本次启动授权内直接执行，`manual` 沿用工具审批。Computer 仅主 Agent 可用，一台电脑同一用户同时只能有一个 ccdp 会话持有桌面控制权；切换时关闭原会话。用 Ctrl+C 中断当前回合后，已完成的动作不会撤销或自动重放；输入事件会成对释放。模型每次收到操作后的截图，最多携带最近两张桌面截图。

首版只操作主显示器；其他显示器、DOM/辅助功能树和后台窗口控制尚未实现。桌面内容通过截图发送给所选模型服务，并作为会话附件保存；`--no-session-persistence` 沿用内存会话。`--computer-use` 每次启动都需显式提供，项目配置、历史会话和子 Agent 无法自动开启它。桌面操作不受项目目录边界限制，因此只在需要时开启；Shell 的文件和网络沙箱保持独立。

## 批量工具调用

Code Mode 默认开启。模型可以通过 `RunCode` 在一次调用里组合读取、搜索和结果处理，再返回需要的汇总。它使用纯 Go 的 Goja 引擎，无需另外安装 JavaScript 运行时；macOS 下支持 `CGO_ENABLED=0` 构建。

每个内部工具仍经过权限与审批检查，执行记录可以在 TUI 中展开查看。需要关闭时，在用户配置或项目设置中添加 `"code_mode_enabled": false`，然后重启或执行 `/reload`。Computer 直接通过专用工具调用。

## 扩展

| 扩展方式 | 配置位置或入口 |
| --- | --- |
| 项目指令 | `AGENTS.md`、`.ccdp/AGENTS.md`、`~/.ccdp/AGENTS.md` |
| Skills | `.ccdp/skills/<name>/SKILL.md`、`~/.ccdp/skills/<name>/SKILL.md` |
| 自定义斜杠命令 | `.ccdp/commands/<name>.md`、`~/.ccdp/commands/<name>.md`，支持 `$ARGUMENTS` |
| MCP | 配置文件的 `mcp_servers`；支持 stdio、Streamable HTTP 和 SSE |
| Hooks | 配置文件的 `hooks`；支持工具调用、输入、压缩和会话生命周期事件 |
| 自定义 shell 工具 | 配置文件的 `tools`；JSON 参数从 stdin 输入，stdout 返回结果 |

项目级 Skills 和自定义命令优先于同名用户级内容。`/skills`、`/mcp` 查看已加载扩展，`/providers` 查看注册的模型 Provider。内置工具与模型直接注册；自定义工具和 MCP 通过配置加载。

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

