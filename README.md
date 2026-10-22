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

Code Mode 使用纯 Go 的 Goja 引擎，支持 `CGO_ENABLED=0` 构建，无需 C 编译器或 Node.js。当前 worker 隔离依赖 macOS Seatbelt，其他平台暂不启用 RunCode。

### Code Mode

Code Mode 默认启用，无需配置，模型即可调用 `RunCode`。只有在 `~/.ccdp/config.json` 或项目设置中显式设置 `"code_mode_enabled": false` 才关闭，重启或 `/reload` 后生效；删除该设置即可恢复默认启用。Chat Completions、Responses 和 Anthropic 都使用同一个本地工具，不需要提供方支持额外的代码执行协议。

模型可以在一次调用里读取多个文件、用结果决定下一次工具参数，再只返回需要的汇总：

```javascript
const rows = await Promise.all(["README.md", "go.mod"].map(async file_path => {
  const result = await tools.Read({file_path});
  return {path: result.data.path, lines: result.data.lines.length, more: !result.data.eof};
}));
text(rows);
```

`tools` 只包含本步骤已向模型公开、允许程序调用的工具，保留原名；MCP 可用 `tools["mcp__server__tool"](...)`。工具返回 `{status, text, data?, truncated}`，失败会拒绝 Promise；必须 `await` 所有调用。`text` 接受 JSON 值，没有调用 `text` 就不会自动返回工具结果。每次运行使用全新 JS 环境。

脚本里的调用继续经过现有权限、审批、hooks 和日志链路。连续内建只读工具可以并发，写入、Bash、网络和 MCP 调用形成顺序屏障。worker 使用独立 Seatbelt 沙箱，不能直接读取项目、访问网络或环境变量。默认时限 120 秒（`timeout_ms` 可设 1–300 秒，人工审批暂停计时），另有 5 秒累计 JS 执行时间、1024 层调用栈、128 次调用、32 项队列、单结果 512 KiB/累计 8 MiB 上限。独立 worker 设置 96 MiB Go 软内存目标，每 10 ms 检查 Go 堆占用，超过 128 MiB 时中断；中断后 250 ms 未退出则终止 worker。这是进程级的采样保护，不是精确的 JS 堆硬上限，大额单次分配可能短暂超出预算。

TUI 显示带父身份的内部调用，展开 RunCode 可查看子调用；恢复只恢复记录，不重新执行脚本。后台 Bash 句柄仍归会话所有，结果里的 `retained_process_ids` 可继续交给 Process。原生图片/音频请直接调用工具。详见 [设计与实现说明](docs/code-mode-design.md)。

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

工作清单随会话保存和恢复，保留任务顺序、状态和优先级。更新未保存成功时，仍显示上一次已保存的清单。

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

以下字段可加入上面的配置对象：

```json
{
  "reasoning_effort": "medium",
  "verbosity": "low",
  "permission_mode": "edits",
  "network_access": true,
  "max_parallel_tools": 4,
  "max_concurrent_agents": 4,
  "enable_project_memory": true,
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

项目覆盖配置位于 `.ccdp/settings.json`，用 `/reload` 重新加载。CLI 和环境变量中的显式设置优先；用 `/config` 检查最终生效值。旧版顶层 `base_url` / `api_key` 和模型数组配置需要迁移到 `providers`。

`max_parallel_tools` 控制同一请求中的工具并发，`max_concurrent_agents` 控制运行中的任务和普通 review 子 Agent；两者独立，默认均为 4，取值范围均为 1–16，0 无效。根 Agent 不计入子任务名额，guardian 使用独立的 1 个名额。排队的子任务不占名额；等待审批、输入和结束清理仍占名额。`/reload` 增加上限立即唤醒排队任务，降低上限保留已运行任务，直到空闲名额足够再启动新任务。

`bash_timeout_seconds` 设置 Bash 命令的默认执行超时；单次调用的 `timeout_ms` 可以覆盖它，显式 `0` 表示不设执行超时。

Bash 每次从 `cwd` 或会话工作目录启动，命令内的 `cd` 和 `export` 不会改变后续工具的环境。传给 Read 的 Bash 产物应使用绝对路径；Read 不展开 `~` 或 shell 变量。`HOME` 和 `TMPDIR` 使用会话临时目录，跨 `/cd`、`/reload` 和沙箱设置更新保留，关闭会话后清理。Bash/Process 按实际 JSON 大小应用 `max_result_size_bytes` 预算；输出未返回完时，用 Process 的 `next_offset`（字节）续读，或用 Read 读取 `log_path`（行号）。进程仍在运行由 `status` 表示，不等于输出被截断。设计对照见 [基础工具设计](docs/basic-tools-design.md)。

</details>

## 记忆

记忆默认开启。Agent 在对话中主动保存长期偏好、用户纠正、确认过的决定和经过验证的经验；直接说“记住……”“把……改成……”“忘记……”即可。模型判断什么值得记住，不会把每轮对话都保存成笔记，也不会在普通聊天后固定增加一次模型调用。任务进度、猜测、密钥和直接从代码就能读到的内容不应进入长期记忆。

记忆分为项目和用户两个范围，默认写项目范围，明确通用于所有项目的偏好才写用户范围。子 Agent 从父 Agent 的任务说明获得相关记忆，由父 Agent 检查并保存返回的经验。

`project_memory_dir` 默认 `~/.ccdp/projects`。目录使用可读路径：Git 项目位于 `git/<公共 Git 目录路径>/memory/`，同一仓库的 worktree 共享；非 Git 项目位于 `workspace/<工作目录路径>/memory/`；用户记忆位于 `user/memory/`。路径开头的 `/` 不保留。

正文保存在独立的 Markdown 文件中；文件头使用 JSON 格式元数据（也是合法 YAML），保存分类、来源和时间。可以直接编辑正文，也支持没有文件头的普通 Markdown。`MEMORY.md` 是生成的简短索引，读取时从最新正文重建，详情按需读取。普通文件工具不能修改记忆目录，模型通过统一的记忆工具写入。

- `/memory` 查看状态、目录和记忆列表；`/memory read <id>` 查看正文与来源。
- `/memory search <关键词>` 搜索项目记忆；模型也可指定用户范围搜索。
- `/memory save <内容>` 手动保存；`/memory save --user <内容>` 保存通用偏好。不需要标题、摘要或版本号。
- `/memory edit <id> <原文> | <新文>` 修改唯一匹配的片段；`/memory delete <id>` 忘记一条记忆。用户范围使用 `user:<id>`。
- `/memory tidy` 合并完全重复的项目笔记并刷新索引；语义合并由模型读取后编辑，避免盲目覆盖。
- `/memory on` 自动读写，`/memory readonly` 只读取，`/memory off` 不读取或生成。设置随会话保存，也可通过 `enable_project_memory`、`memory_read_only` 配置默认值。`--no-session-persistence` 下只读。
- `/memory history on` 开启当前会话的可选历史提取，`off` 关闭；配置项为 `memory_history`，默认关闭。开启后后台在空闲至少一分钟时检查，检查间隔至少五分钟，分批处理已完成且未处理的回合；恢复会话后继续处理进度。需要调用模型，但不阻塞正常聊天。
- `/memory extract [session-id]` 手动触发一批历史提取，省略 ID 时使用当前会话。只接收同一项目的根会话，跳过关闭记忆、只读、未完成、撤回和过大的回合；自动提取还跳过当时未开启历史提取的回合。

历史提取每批处理最多四个回合，将已有项目和用户记忆的完整正文、来源与更新时间一起交给模型，合并相关信息或补录独立事实；只写项目记忆。普通保存不再跳过整个来源回合，每回合最多四条提议，每批最多十六条；同一已有条目的修改必须合并成一条提议。正文输入超过 128 KiB 时会报错并保留进度，不截断已有纠正信息。语义匹配由模型判断，新增内容与已有条目完全重复时由存储层再次去重。

所有写入使用同一文件锁和原子替换；编辑匹配原文，无须用户提供 revision 或内容哈希。历史整理使用独立锁，慢模型不会阻塞显式修改。提交前重新核对项目和用户记忆快照；期间发生修改就放弃本批结果，保留进度供重试。整批先检查目标、内容和容量，再逐文件原子保存；若中途发生 I/O 错误，下次会结合已保存的条目重新整理。显式纠正、删除仍会保守排除旧来源回合，避免重新导入旧内容；删除后只保留来源标记，不保留正文。直接从文件系统删除文件无法保留该标记。

旧记录中无法区分普通保存与纠正的已修改条目，继续保守排除其旧来源；新记录单独保存排除标记。历史内容的先后按来源回合判断，记忆文件的更新时间不代表原始证据的发生时间。

每个范围最多 64 条有效记忆、每条 8 KiB、合计 256 KiB；摘要最多 4 KiB。记忆读取失败会提示并继续聊天，保存失败会明确报错；关闭或改为只读会取消正在运行的历史提取。默认使用 Markdown 新格式，旧 JSON 笔记不自动迁移。

会话记录使用 SchemaVersion 3；旧记录不自动恢复，原文件保留。

## 权限与沙箱

审批模式决定哪些操作需要人工确认，默认是 **`edits`**。

| 模式 | 行为 |
| --- | --- |
| `manual` | 文件修改、非只读命令等需要审批 |
| `edits` | 自动接受文件编辑，非只读命令等仍需要审批 |
| `bypass` | 跳过普通审批；明确拒绝、plan 约束和沙箱限制仍生效 |

plan 是独立工作流：先以只读工具探索并提出计划，批准后执行。`always_deny` 优先于 `always_allow`。未经显式规则授权的原始 Bash Git 命令需要审批；`/git` 提供隔离执行的只读查询。

所有模型驱动的本地命令、后台进程、Hooks 和 stdio MCP 都经过 macOS Seatbelt。工作区默认可读写，工作区外的主机文件默认可读、不可写；`disallowed_directories` 可明确拒绝读写。审批命令不会自动扩大目录或网络授权。

`sessions` 目录默认可读，包括历史会话和工具保存的完整输出；它保留写保护，避免模型修改会话状态。保存项目审批模式的 `project-permissions` 目录按项目绝对路径的层级保存 `permission.json`（去掉路径开头的 `/`），同样禁止模型写入，即使自定义会话目录位于工作区内。用户显式配置的禁读目录仍然生效。代码审查复用普通子任务的工作区、工具和审批机制，审查目标及报告格式通过任务指令指定。

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

工具输出额度使用字节单位：`max_result_size_bytes` 控制单个结果，`max_tool_output_bytes_per_turn` 控制整轮累计结果。未信任项目可以降低 `max_parallel_tools` 和 `max_concurrent_agents`，提高额度需要项目信任；环境变量和 CLI 的显式设置保持优先。
