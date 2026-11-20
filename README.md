# ccdp

在终端里和 agent 一起完成开发任务。用 Go 编写，连接你选择的模型 API，在本地项目中工作。

给 ccdp 一个任务，它会读取代码、修改文件、运行命令并检查结果。你可以在执行中补充要求、调整方向或随时中断；下次打开时，从保存的会话继续。

ccdp 将对话运行时与本地执行环境分开。默认的 `auto` 模式让已授权范围内的开发工作直接进行，需要额外访问时再询问。项目约定写在 `AGENTS.md` 中，已有的工具和流程可以通过 Skills、MCP 与 Hooks 接入。

[开始使用](#开始使用) · [macOS 桌面端](#macos-桌面端) · [使用指南](docs/usage.md) · [开发](#开发)

## 开始使用

目前支持 **macOS**，需要支持流式输出和工具调用的模型 API。桌面应用自带 ripgrep。从源码运行需要 Go 1.27.3+，命令行版的文件搜索使用本机 ripgrep，可通过 `brew install ripgrep` 安装。

### 1. 安装

从源码构建并安装：

```bash
git clone https://github.com/cointem/ccdp.git
cd ccdp
go install ./cmd/ccdp
```

将 Go 的安装目录加入 `PATH`，默认是 `$(go env GOPATH)/bin`。也可以用 `go build -o ccdp ./cmd/ccdp` 构建，在当前目录运行 `./ccdp`。

### 2. 连接模型

创建 `~/.ccdp/config.json`。下面是 OpenAI 兼容 Chat Completions 服务的配置模板；将地址和模型名换成你使用的服务，窗口与输出上限按服务商说明填写：

```json
{
  "model": "my-provider/my-model",
  "providers": {
    "my-provider": {
      "base_url": "https://your-api.example/v1",
      "api_key_env": "MY_MODEL_API_KEY",
      "wire_api": "chat",
      "models": {
        "my-model": {
          "context_window": 128000,
          "max_output_tokens": 8192
        }
      }
    }
  }
}
```

`my-provider` 是自定义的供应商名称，`my-model` 是服务商的模型 ID。示例中的数值也需要按实际模型调整。在启动 ccdp 的终端里提供密钥：

```bash
export MY_MODEL_API_KEY="你的 API Key"
```

也支持 Responses 和 Anthropic Messages 协议。多模型配置、协议选择与本地服务接入见[模型配置](docs/usage.md#模型配置)。

### 3. 开始一个任务

进入你要工作的项目：

```bash
cd /path/to/your/project
ccdp
```

然后直接输入任务：

```text
检查登录接口的错误处理，修复发现的问题，并运行相关测试。
```

输入 `/help` 查看操作，`/model` 切换模型，`/config` 检查生效配置。启动时直接使用当前权限配置，默认 `auto`；需要逐次审批修改和命令时，用 `ccdp --mode manual` 启动。

## macOS 桌面端

Apple Silicon 用户可从 [GitHub Releases](https://github.com/cointem/ccdp/releases) 下载 macOS ZIP，解压后将 `ccdp.app` 拖入「应用程序」。

桌面端使用 [MyGo](https://mygo.run/) 原生 Go UI，采用侧栏、会话工作区和底部输入框布局，支持浅色与深色外观。它与终端共用 `~/.ccdp/config.json`、历史会话及权限策略。

点最左侧导航栏底部的「设置」或按 `⌘,` 打开配置页。选择供应商可填写 API 地址、协议、密钥或环境变量，并管理模型。「通用」里可以选择默认模型、备用模型和权限模式。按 `⌘S` 保存后，草稿和模型列表立即更新。已有会话保留仍可用的当前模型，从下一次请求起使用新配置。

打开项目后，点项目名旁的「… → 编辑项目」可改侧栏显示名、查看工作目录和会话数。在「项目指令」里可以编辑仓库的 `AGENTS.md` 或 `.ccdp/AGENTS.md`。项目行的「+」在该目录新建对话，置顶、折叠和显示名会在下次启动时恢复。

「自定义」中的「插件」列出已安装的扩展，可从本地目录安装技能型插件、查看所含技能和卸载。「MCP」单独列出服务，可添加连接、编辑配置和卸载，包括默认安装的 Exa。从「设置 → MCP」也能进入同一管理页。「技能」可以按名称或说明搜索、按个人或项目筛选，也可以选择含 `SKILL.md` 的目录添加技能。未打开项目时也可以查看个人安装。

本地技能型插件的根目录包含 `.codex-plugin/plugin.json` 和 `skills/<技能名>/SKILL.md`。桌面端将插件安装在 `~/.ccdp/skills/.plugins/` 中，桌面端与终端共用这些技能。MCP 服务、Hooks、自定义工具及其信任状态可从项目菜单的「当前项目的扩展」查看。

从源码启动，`-d` 指定要工作的项目：

```bash
go run ./cmd/ccdp-desktop -d /path/to/your/project
```

也可以用 `-r <session-id>` 恢复会话，或用 `-config-file <path>` 指定配置。界面支持打开项目、搜索及切换历史会话、流式 Markdown 和代码高亮、工具详情、模型切换、权限审批、回答问题与查看子任务。`Enter` 发送，`Shift+Enter` 换行，`⌘N` 新建会话，`⌘O` 打开项目。

构建可双击运行的 macOS 应用：

```bash
sh scripts/build-macos.sh
open build/desktop/darwin-arm64/ccdp.app
```

macOS 应用内置 ripgrep 15.2.0，可离线搜索。首次打包会从 ripgrep 官方发行版下载对应架构的工具，从 ccdp Release 下载图标，并校验 SHA-256。再次构建可复用已校验的本地资源。MyGo 将 `rg` 打包到 `Contents/Resources/bin`，同时附带许可证。

输出目录中的架构跟随构建机器；Intel Mac 为 `darwin-amd64`。默认使用本地 ad-hoc 签名；分发签名和公证可在 `mygo.json` 中配置。通过 Finder 启动不会继承终端的临时 API Key 环境变量；使用 `api_key_env` 时可从配置好环境的终端启动。

这是首版桌面客户端。图片附件、终端斜杠命令面板和独立 diff 审阅界面尚未接入；这些操作可继续使用终端端。实现边界和测试方式见 [desktop README](internal/desktop/README.md)。

## 在任务中继续交流

运行中按 `Enter` 补充当前任务，按 `Tab` 将消息留到下一轮处理；补全菜单打开时，`Tab` 优先补全。快速连按两次 `Esc` 清空草稿；空输入时按 `Esc`，或按 `Ctrl+C` 中断执行。较长的要求可以用 `Ctrl+G` 交给编辑器，图片用 `Ctrl+V` 粘贴。

会话会自动保存。离开后运行 `ccdp -c` 选择历史会话；想从某个节点尝试另一条思路，可以在会话中使用 `/fork`。对话回退与文件恢复是两件事，操作方式见[日常使用](docs/usage.md#日常使用)。

也可以把 ccdp 放进脚本：

```bash
ccdp -p "检查当前改动" --output-format json
```

脚本运行使用相同的权限配置，需要交互审批的操作会被拒绝。持续执行的目标、预算和流式输入见[脚本运行](docs/usage.md#脚本运行)。

## 适应你的项目

把构建方式、测试命令和代码约定写进项目的 `AGENTS.md`，让每次会话从相同的约定出发。反复使用的工作流程可以写成 Skill；外部工具通过 MCP 接入；需要在工具执行前后检查或处理结果时，使用 Hook。配置位置与示例见[扩展](docs/usage.md#扩展)。

需要操作桌面时，显式用 `ccdp --computer-use` 启动。首版支持 macOS 14+ 的主显示器，需要支持图片输入的模型和系统授权；截图会发送给所选模型服务。构建要求和权限说明见[桌面操作](docs/usage.md#桌面操作)。

更多操作见[使用指南](docs/usage.md)，包括上下文管理、记忆、子任务、持续目标和权限设置。

## 开发

在源码目录中运行：

```bash
go build ./cmd/ccdp
go test ./...
go vet ./...
```

完整测试需要可用的 macOS Seatbelt 和本地 HTTP 监听能力，须从宿主终端启动。CCDP 的 Bash 已处于 Seatbelt 中，不能在其中再次初始化执行器沙箱；增加临时目录授权无法解决这个限制。普通项目的构建和测试仍可直接使用 Bash。

修改沙箱或执行环境时，运行完整的真实后端检查：

```bash
CCDP_SEATBELT_INTEGRATION=1 go test ./...
go test -race ./internal/runtimepath ./internal/fsops ./internal/sandbox ./internal/execution
```

这些检查覆盖目录准备、文件访问、已安装工具链、进程回收、本机 IPC 和跨会话隔离。涉及并发与生命周期的改动还应运行相关竞态测试。

反馈问题请提交 [GitHub Issue](https://github.com/cointem/ccdp/issues)，附上运行命令、错误信息和脱敏后的配置。
