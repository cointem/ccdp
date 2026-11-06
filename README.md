# ccdp

在终端里和 agent 一起完成开发任务。用 Go 编写，连接你选择的模型 API，在本地项目中工作。

给 ccdp 一个任务，它会读取代码、修改文件、运行命令并检查结果。你可以在执行中补充要求、调整方向或随时中断；下次打开时，从保存的会话继续。

ccdp 将对话运行时与本地执行环境分开。默认的 `auto` 模式让已授权范围内的开发工作直接进行，需要额外访问时再询问。项目约定写在 `AGENTS.md` 中，已有的工具和流程可以通过 Skills、MCP 与 Hooks 接入。

[开始使用](#开始使用) · [使用指南](docs/usage.md) · [开发](#开发)

## 开始使用

目前支持 **macOS**。需要 Go 1.27+、[ripgrep](https://github.com/BurntSushi/ripgrep)，以及支持流式输出和工具调用的模型 API。尚未安装 ripgrep 时，可运行 `brew install ripgrep`。

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

首次进入开发环境时，ccdp 会确认工作区写入和网络访问范围；相同范围下的后续启动会复用这次决定。输入 `/help` 查看操作，`/model` 切换模型，`/config` 检查生效配置。需要逐次审批修改和命令时，用 `ccdp --mode manual` 启动。

## 在任务中继续交流

运行中按 `Enter` 补充当前任务，按 `Tab` 将消息留到下一轮处理；补全菜单打开时，`Tab` 优先补全。按 `Esc` 或 `Ctrl+C` 中断执行。较长的要求可以用 `Ctrl+G` 交给编辑器，图片用 `Ctrl+V` 粘贴。

会话会自动保存。离开后运行 `ccdp -c` 选择历史会话；想从某个节点尝试另一条思路，可以在会话中使用 `/fork`。对话回退与文件恢复是两件事，操作方式见[日常使用](docs/usage.md#日常使用)。

也可以把 ccdp 放进脚本：

```bash
ccdp --mode auto -p "检查当前改动" --output-format json
```

无交互运行时，`--mode auto` 明确接受当前开发环境的基础范围；需要额外授权的操作仍会被拒绝。持续执行的目标、预算和流式输入见[脚本运行](docs/usage.md#脚本运行)。

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

完整测试需要可用的 macOS Seatbelt 和本地 HTTP 监听能力。涉及并发与生命周期的改动还应运行相关竞态测试。

反馈问题请提交 [GitHub Issue](https://github.com/cointem/ccdp/issues)，附上运行命令、错误信息和脱敏后的配置。
