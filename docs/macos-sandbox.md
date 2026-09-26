# macOS 统一沙箱

本文定义 CCDP 的完整 macOS 沙箱契约，不按开发阶段划分能力。实现与测试必须满足下面的边界；局部测试结果不改变设计。

## 1. 决策

仅适配 macOS，固定使用 Seatbelt 后端。沙箱没有 `confine` / `strict` / `none` 三档，也没有 `sandbox_mode` 选择器、旧值映射或兼容读取。所有模型驱动的本地命令和外部程序始终进入 Seatbelt，不提供日常关闭或自动无沙箱重试入口。后端不可用或 profile 无法加载时阻止执行，TUI 仍可显示诊断、配置和历史。旧 `SandboxPolicy.mode` / `allow_network` 字段会被协议拒绝；配置和会话不会把旧档位迁移为新策略。

Harness 与沙箱按职责及信任边界分离，不拆成独立服务：CCDP 主进程拥有 agent loop、模型调用、上下文、permission、会话和工具调度；`internal/execution` 是主进程中的统一执行适配层；Seatbelt 限制被启动的工具子进程。内置文件工具保留在主进程，由 permission 和统一路径策略检查控制，不声称其 IO 已受 Seatbelt 隔离。Web 工具和远程 MCP 的 HTTP 通信也由主进程执行，单独经过网络能力检查。

策略字段只表示具体访问授权：`NetworkAccess`、`AdditionalDirectories`（读写）、`AdditionalReadOnlyDirectories`、`DisallowedDirectories` 和 `Revision`。不保留旧字段解码、旧值映射、弃用期、双轨实现或自动数据转换；磁盘上的既有用户历史数据不会因此删除。

保留现有 permission 系统：它决定一次操作是否获准；沙箱决定获准操作能够访问什么。`bypassPermissions` 只跳过常规操作审批，不能关闭沙箱、扩大目录或网络授权，也不能越过明确拒绝、Plan 限制和父代理权限上限。

不引入 Linux 后端、容器、虚拟机、远程执行服务或新的权限模式。需要无隔离操作时由用户在外部终端执行。

## 2. 单一默认访问策略

| 资源 | 默认规则 |
| --- | --- |
| 工作区 | 可读写，排除下述控制文件和明确禁止路径 |
| 系统运行时、系统命令 | 允许启动 shell、加载动态库和执行工具所需的读取、映射和进程操作 |
| 开发工具链 | 默认可读；发现实际使用的安装根和 SDK 仍可帮助执行器配置工具环境，但不扩大写权限 |
| 工作区外的用户数据 | 默认可读、不可写；明确拒绝目录仍不可读写，额外写入按具体目录授权 |
| 临时目录、构建缓存 | 为 session 创建独立目录；显式设置 TMPDIR 和支持的工具缓存变量；不开放全局 /tmp |
| CCDP 配置、项目 .ccdp、信任记录、会话存储 | 普通工具不可写；会话数据默认不可读，配置内容按需由主进程提供经过筛选的视图 |
| Git 控制文件 | 保护 config、hooks 及 worktree 对应的真实控制路径；普通代码修改、index 和提交对象写入仍可运行 |
| 子进程网络 | 默认禁止 TCP/UDP 外联及访问主机本地服务；联网和本地服务访问分别按需授权 |
| 凭证 | 不自动继承 provider 密钥、主机凭证和代理 socket；确需凭证的适配器使用显式、最小授权 |

这是一个固定基线加具体能力授权，不再命名为多个“安全等级”。默认文件读取范围与 Codex 的 `workspace-write` 一样覆盖主机可读文件；这不授予额外写入权限，也不能越过操作系统权限。额外读写、只读和拒绝目录以具体 root 表达；只读目录主要用于在工作区或额外可写目录内划出禁止写入的区域，位于可写根外时不改变默认读取行为；拒绝目录优先禁止读写。既有 `additional_directories` 的读写含义不变。长期额外写入授权来自用户配置，不写进可被模型修改的项目文件。本地会话也可通过明确审批取得内存态 once/session capability。项目设置只允许收紧：显式 `network_access: false` 可以关闭外联，`disallowed_directories` 只能累加，不能增加目录或联网授权；项目受信不会改变这条限制。

`network_access` 为本地进程提供 TCP/UDP 外联能力，但不会同时打开本机服务监听；本机服务连接或监听单独按 TCP/UDP 和端口授权。Seatbelt 的 `localhost:<port>` selector 可限制端口，但不能保证授权只匹配字面上的 `127.0.0.1` 或只匹配某个 loopback 接口。Unix-domain socket 不随外联授权开放。

文件路径策略同时用于内置文件工具的应用层检查和子进程的 Seatbelt profile，但二者的强制执行机制不同。两者都默认允许主机文件读取并执行明确拒绝目录；系统运行时的执行例外不改变这些拒绝规则。用户数据授权由一个不可变策略对象统一生成，两类适配器只增加各自必要的运行时例外；相同的授权规则不等于相同的隔离强度。

## 3. permission 与能力授权

沿用现有审批组件，在一次请求内展示操作及所需权限，不建立第二套审批 UI。

例如 `npm install` 需要执行许可和联网能力；批准该请求时可以同时批准两者。只批准命令本身，不代表批准所有网络或任意文件访问。自动批准规则也不能隐式扩大沙箱能力。

授权范围只提供“本次调用”和“本会话”；目录写入请求注明范围，网络请求注明允许的范围。默认读取无需额外目录授权；只读目录授权可以作为重叠可写根的写入排除规则，但不能覆盖明确拒绝目录。`once` 只进入本次调用的策略快照；`session` 授权保存在当前运行时内存中，恢复会话时不从历史批准记录恢复。长期授权通过用户配置中的具体目录和网络设置维护。内置工具可从参数推导所需路径；主进程验证结构化权限请求，模型不能直接生成生效策略。

网络模型不引入域名代理：子进程外联授权是整个调用的外联能力，不是某个命令名或某个域名的保证。入站监听不随外联自动开放；本机服务按 protocol、方向和具体端口授权，但 `localhost` selector 不能强制绑定到指定 loopback 字面地址。Unix socket 默认不开放；Docker、SSH agent 等可把行为转交宿主执行的 socket 不作为普通网络权限自动放行。

不能可靠归因的 `EPERM`、非零退出码，只报告操作失败，不一律解释成沙箱拒绝。禁止先试运行任意命令、失败后自动放宽权限并重试：失败前可能已经产生副作用。带新授权的再次执行使用新的调用记录，并提示已有部分结果。

## 4. Harness 与执行边界

```text
CCDP 主进程：harness
  模型调用 / 上下文 / permission / 会话 / 工具调度
    → 验证操作许可和具体能力授权，冻结策略 revision
    ├─ 内置文件工具 → 应用层路径检查 → 主进程文件 IO
    ├─ Web / 远程 MCP → 适配器权限检查 → 外部服务
    └─ execution → /usr/bin/sandbox-exec
                     └─ shell / argv 工具 / hooks / stdio MCP
```

`internal/sandbox` 生成 SBPL，`internal/execution` 负责策略快照、进程启动、取消、输出限制和生命周期标记。Bash、`ProcessStart`、hooks、stdio MCP、git/gh 适配器及搜索工具启动的外部进程都使用固定的 `/usr/bin/sandbox-exec`。shell 命令在 Seatbelt 内运行 `/bin/sh -c`；argv 调用直接传参数。Seatbelt 或 profile 加载失败会停止执行，不会回退到宿主 shell。

固定使用 `/usr/bin/sandbox-exec`，不通过 PATH 查找包装器。系统工具、配置的 shell 和工具链路径须规范化；尚不存在的受保护路径也要解析最近的现存父目录，避免 `/tmp` 等路径别名绕过。受保护/拒绝目录同时排除路径本身和子路径，并禁止重命名其可写祖先目录，以免将受限内容移出路径范围。可写授权根本身也不能被工具替换为另一个目录。保留 deny-default、受保护路径优先、策略加载失败即停止；正式执行失败不能降级。

模型可达的本地进程入口统一进入 execution；用户主动触发的剪贴板、打开链接等主机 UI 功能留在主进程，不属于模型驱动命令执行。

Read/Write/Edit/Glob/Grep 在主进程执行文件 IO，不引入文件 worker 或 IPC 协议。它们统一使用路径策略，检查授权目录、禁止路径和控制文件保护，保留文件版本状态与原子写语义。若搜索工具内部调用 rg 等外部程序，该子进程仍必须经过 execution 和 Seatbelt；不能因为工具名是文件搜索就豁免。

路径校验不能把“检查路径后 os.Open”变成内核隔离。文件工具在主进程内执行应用层路径检查；Seatbelt 主要隔离外部程序及它们的子进程。路径穿越、symlink/hardlink 控制文件别名、原子写和并发替换风险由独立测试覆盖；主进程文件工具对可能指向明确拒绝目录的多重链接采取保守拒绝。Seatbelt 按路径匹配，无法从已开放读取范围内的硬链接反查它在拒绝目录中的别名；这类文件按当前路径的权限处理。应用层校验不代表消除所有竞态。子进程仅继承必要文件描述符。

不引入文件 worker 是明确的信任边界选择：主进程的内置文件工具是受信实现，依靠路径策略约束；Seatbelt 隔离不可信外部程序及其后代。任意第三方代码若进入主进程，则不能宣称本设计能隔离该代码。

## 5. 主进程、Web 和 MCP

主进程保留模型 API、TUI、持久化及审批能力，不整体放进工具沙箱。子进程禁网不意味着 CCDP 无法连接模型 API。

WebFetch/WebSearch 和远程 SSE MCP 工具调用使用主进程网络；调用前由 capability policy 检查 outbound 授权，工具实现也会拒绝未带授权的调用。远程 MCP server 的连接启动还要求用户在该 server 配置中显式设置 `network_authorized: true`；它只同意连接配置的 server，不取代每次工具操作的 permission 或 network capability。普通工具操作许可仍单独判定。远端副作用不受本机 Seatbelt 约束；远程配置连接的启动行为不能被误描述为本地 Seatbelt 隔离。

stdio MCP 作为本地子进程进入 Seatbelt；实例绑定启动时的 sandbox snapshot。给某个 MCP 的显式凭证仅传给该进程，不合并进通用 shell 环境。默认去除 SSH_AUTH_SOCK 等可借用主机权限的入口。

## 6. 生命周期与撤销

每次调用绑定不可变策略及授权 revision；once 授权仅适用于该调用，session 授权保存在当前会话的内存态。恢复会话时不会恢复以前的 once/session 能力。

执行器维护由父会话和子 agent 共享的 sticky external-execution witness。只要本会话启动过任意本地进程，该事实就不会因进程正常退出或重试而清除。进程组取消会回收通常的子进程，但不能证明通过 `setsid` 脱离进程组的后代已退出；Seatbelt 限制仍会随进程继承，这不等于该进程已被结束。诊断显示的是“可能仍有旧 profile 后代”的保守标志，不是准确 PID 清单。

sticky witness 本身不阻止正常派发或新增授权。用户发起 `/sandbox revoke all`，或发起会缩紧旧策略的设置变更后，运行时会关停托管的后台进程、当前 turn、子 agent 和 MCP；若无法确认所有可能保留旧 profile 的执行体都已退出，会进入 `revocationPending`，继续阻断工具派发和设置操作。再次 retry 不会清除 sticky witness；需独立清理可能残留的进程并新建会话。Seatbelt 无法事后修改已运行进程的策略，因此不宣称可以热撤销全部进程权限。

普通会话关闭会取消并等待受管理的进程 leader/进程组、关闭 MCP、清理 scratch；若 sticky witness 已置位，会诊断说明 detached descendants 的退出状态无法确认。关闭不会清除历史风险事实。新增授权本身不触发该撤销阻断；只有进入 revocation pending 后才暂停派发和设置。

## 7. 配置和界面

无沙箱档位、兼容字段或自动迁移。protocol `SandboxPolicy` 字段为 `NetworkAccess`、`AdditionalDirectories`、`AdditionalReadOnlyDirectories`、`DisallowedDirectories`、`Revision`；TUI 的 `/mode` 仍只表示 permission mode。

`/sandbox` 显示只读诊断：Seatbelt backend、策略 revision、workspace、额外读写/只读/拒绝 roots、网络授权、session-only grants、revocation pending 和 sticky external-execution 状态；该标志不是准确的 detached PID inventory。`/sandbox revoke all` 会尝试停止本 session 的 turn、子 agent、托管后台进程和 MCP，并撤销内存态 session grants。若 sticky witness 阻止确认撤销，诊断会显示 pending 且新派发保持阻断。

只支持 macOS：非 macOS 可以检查配置或历史，发起本地执行时报不支持，不回退到应用层“弱沙箱”。

## 8. 验收条件

- 所有模型驱动的本地执行入口及其后代都进入真实 macOS Seatbelt；后端或 profile 失败时拒绝执行。
- 工作区和授权目录内的正常构建、Git 操作可用；工作区外默认读取可用，明确拒绝目录不可读写，未授权写入、受保护路径首次创建、symlink 目录项替换、祖先重命名及可写授权根替换受到阻止。只读目录与可写根重叠时仍不可写；Git config/hooks/worktree 指针受保护，index/object/ref 与提交仍可运行。
- 默认网络拒绝；广域外联不会解锁 loopback、本机监听或 Unix socket；本机服务只按授权协议、方向和端口生效。
- 主进程的 Read/Write/Edit/LS/Glob/Grep、Web 与远程 MCP 分别经过路径或网络能力检查和 permission；目录枚举不能返回或进入拒绝子树，主进程工具不被误称为 Seatbelt 隔离。
- 并发授权、排队调用、热加载、撤销及后台后代维持 revision 和 `revocationPending` 语义；撤销无法确认时持续阻断。

## 9. 架构参考

[Codex Seatbelt 实现](https://github.com/openai/codex/blob/main/codex-rs/sandboxing/src/seatbelt.rs)提供受保护路径本身与可写祖先的处理参考；[Claude Code 沙箱说明](https://code.claude.com/docs/en/sandboxing)和 [Anthropic sandbox-runtime](https://github.com/anthropics/sandbox-runtime)提供 harness 与 OS 沙箱、网络代理的职责参考。CCDP 固定使用上述本地 macOS 边界，不引入沙箱档位、域名代理或自动无沙箱回退。
