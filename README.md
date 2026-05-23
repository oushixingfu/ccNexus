<div align="center">

<p align="center">
  <img src="docs/images/ccNexus.svg" alt="ccNexus: Claude Code、Codex CLI、Hermes Agent 与 OpenClaw API Provider 热切换中枢" width="720" />
</p>

[![构建状态](https://github.com/oushixingfu/ccNexus/actions/workflows/build.yml/badge.svg)](https://github.com/oushixingfu/ccNexus/actions)
[![许可证: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go 版本](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev/)

[English](docs/README_EN.md) | [简体中文](README.md)

</div>

# ccNexus

ccNexus 是一个给 AI 编程工具用的本地 API 中转站。你只需要把 Claude Code、Codex CLI、Hermes Agent、OpenClaw 或 CC Switch 指向 ccNexus，后面多个上游 API、多个 Key、Codex Token Pool、模型切换、故障转移和统计都交给 ccNexus 管。

它适合这几类场景：

- 你有多个 API 上游，希望一个坏了自动换另一个。
- 你想让 Claude Code、Codex CLI、Hermes Agent 共用同一套上游配置。
- 你想通过 Web UI 管理 Key、模型、Token Pool、用量统计和备份。
- 你已经在用 [CC Switch](https://cc-switch.cc/)，希望把 ccNexus 当成一个稳定 Provider 接进去。

> [!IMPORTANT]
> 当前 fork 主要维护 server/Web UI 方向。推荐用本机 server 或 Docker 方式长期运行；`cmd/desktop` 里的 Wails 桌面端源码保留用于历史兼容，不是当前推荐安装方式。

## 新手先看这个流程

```text
1. 先启动 ccNexus
2. 打开 Web UI
3. 在 Web UI 里添加至少一个上游端点
4. 选择一种接入方式：
   A. 已经使用 CC Switch：把 ccNexus 加成 CC Switch 的自定义 Provider
   B. 不使用 CC Switch：把 Claude Code / Codex CLI / Hermes Agent 直接指向 ccNexus
5. 测试一次对话，请求成功后再继续加更多端点
```

第一次配置时建议先只加一个稳定端点。确认客户端能跑通以后，再添加备用端点、Token Pool、WebDAV/S3 备份和更多高级策略。

## 1. 启动 ccNexus

### 方式一：Docker Compose，推荐给大多数用户

需要先安装 Docker。

```bash
cd cmd/server
```

新建或编辑 `cmd/server/.env`，至少写入一个管理密码：

```env
CCNEXUS_BASIC_AUTH_PASSWORD=换成一个强密码
```

启动：

```bash
docker compose up -d --build
```

默认访问地址：

```text
Web UI:  http://127.0.0.1:3021/ui/
健康检查: http://127.0.0.1:3021/health
```

如果 `3021` 被占用，修改 `cmd/server/docker-compose.yml` 里的端口映射，例如：

```yaml
ports:
  - "127.0.0.1:3022:3000"
```

改成 `3022` 后，下面所有示例里的 `http://127.0.0.1:3021` 都要同步换成 `http://127.0.0.1:3022`。

### 方式二：本机 Go 运行，推荐给开发或临时试用

需要 Go 1.24 或更高版本。

```bash
cd cmd/server
go run . -port 3000
```

默认访问地址：

```text
Web UI:  http://127.0.0.1:3000/ui/
健康检查: http://127.0.0.1:3000/health
```

### 方式三：构建 server 二进制

```bash
cd cmd/server
go build -ldflags="-s -w" -o ccnexus-server .
./ccnexus-server -port 3000
```

## 2. 在 Web UI 里添加上游端点

打开 Web UI 后进入端点管理页面，点击「添加端点」。新手只需要先理解 5 个字段：

| 字段 | 怎么填 |
|------|--------|
| 名称 | 自己能看懂即可，例如 `openai-main`、`deepseek-backup` |
| API 地址 | 上游 API 地址，例如 `https://api.openai.com/v1` |
| API Key | 上游给你的 Key；如果使用 Codex Token Pool，这里通常不用填 |
| 模型 | 这个端点实际要用的模型名 |
| 转换器 | 告诉 ccNexus 上游是什么协议 |

常用转换器选择：

| 上游类型 | 推荐转换器 | 适合客户端 |
|----------|------------|------------|
| Claude / Anthropic 兼容 | `claude` | Claude Code、Hermes Claude 模式 |
| OpenAI Chat Completions 兼容 | `openai` | OpenAI 兼容客户端、Hermes OpenAI 模式 |
| OpenAI Responses API 兼容 | `openai2` | Codex CLI，推荐 |
| Gemini | `gemini` | Gemini 兼容上游 |
| DeepSeek | `deepseek` | DeepSeek 兼容上游 |
| Kimi / Moonshot | `kimi` | Kimi 兼容上游 |

### 使用 Codex Token Pool

如果你使用的是 Codex Token Pool：

1. 在端点认证方式里选择 `Codex Token Pool`。
2. 到 Token Pool 页面导入 token JSON，支持 `access_token` 和 `refresh_token`。
3. ccNexus 会自动适配 ChatGPT Codex 后端、自动轮换 token、刷新 401、隔离失效 token，并记录额度快照和用量。

## 3. 选择接入方式

下面二选一即可。

### 方式 A：接入 CC Switch

适合已经在用 CC Switch 管理 Claude Code、Codex CLI、Gemini CLI、MCP、Skills 或 Prompt 模板的用户。

思路是：

```text
Claude Code / Codex CLI / Hermes
        |
        v
    CC Switch
        |
        v
    ccNexus
        |
        v
  你的多个上游 API / Token Pool
```

在 CC Switch 里新增一个自定义 Provider，建议按协议分成两个 Provider，避免路径填错。

### 给 Claude Code 或 Hermes Claude 模式使用

| 配置项 | 值 |
|--------|----|
| Provider 名称 | `ccNexus Claude` |
| Provider 类型 | Anthropic / Claude 兼容 |
| Base URL | `http://127.0.0.1:3021` |
| API Key / Token | 随便写一个非空值，例如 `dummy` |
| Model | ccNexus 端点里配置的 Claude 模型名 |

注意：Claude / Anthropic 兼容入口不要在 Base URL 后面加 `/v1`。

### 给 Codex CLI 或 OpenAI 兼容模式使用

| 配置项 | 值 |
|--------|----|
| Provider 名称 | `ccNexus OpenAI` |
| Provider 类型 | OpenAI 兼容 |
| Base URL | `http://127.0.0.1:3021/v1` |
| API Key | 随便写一个非空值，例如 `dummy` |
| Model | ccNexus 端点里配置的 OpenAI / Responses 模型名 |

如果 CC Switch 里能选择 API 类型，Codex CLI 优先选择 Responses API；对应 ccNexus 端点转换器用 `openai2`。

配置完成后，在 CC Switch 中把当前 Provider 切到 `ccNexus Claude` 或 `ccNexus OpenAI`，再启动对应客户端测试。

### 方式 B：客户端直接接 ccNexus

适合不使用 CC Switch，或者想先最小化跑通的人。

下面假设你用 Docker Compose，ccNexus 地址是：

```text
http://127.0.0.1:3021
```

如果你用的是 Go 本机运行，请改成：

```text
http://127.0.0.1:3000
```

### Claude Code，简称 cc

编辑：

```text
~/.claude/settings.json
```

写入或合并下面的 `env`：

```json
{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "dummy",
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:3021",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000"
  }
}
```

然后重启 Claude Code。Claude Code 走的是 Claude / Anthropic 兼容入口，所以 Base URL 不加 `/v1`。

### Codex CLI

编辑：

```text
~/.codex/config.toml
```

推荐 Responses API：

```toml
model_provider = "ccNexus"
model = "gpt-5-codex"
preferred_auth_method = "apikey"

[model_providers.ccNexus]
name = "ccNexus"
base_url = "http://127.0.0.1:3021/v1"
wire_api = "responses"
```

如果你的上游只支持 Chat Completions，把 `wire_api` 改成：

```toml
wire_api = "chat"
```

同时在 ccNexus 对应端点上选择 `openai` 转换器。Responses API 场景推荐使用 `openai2` 转换器。

### Hermes Agent

Hermes Agent 按你选择的协议来填：

| Hermes 模式 | Base URL | ccNexus 转换器 |
|-------------|----------|----------------|
| Claude / Anthropic 兼容 | `http://127.0.0.1:3021` | `claude` |
| OpenAI 兼容 | `http://127.0.0.1:3021/v1` | `openai` 或 `openai2` |

API Key 可以填 `dummy`。真正访问上游用的是 ccNexus 端点里保存的 Key 或 Token Pool。

## 4. 验证是否成功

先检查 ccNexus 是否在线：

```bash
curl http://127.0.0.1:3021/health
```

再检查模型接口：

```bash
curl http://127.0.0.1:3021/v1/models
```

最后打开你的客户端发一条简单消息。如果失败，按这个顺序排查：

1. Web UI 里当前端点是否启用。
2. 当前端点模型名是否和客户端配置一致。
3. Claude / Anthropic 兼容入口是否误加了 `/v1`。
4. OpenAI / Codex 兼容入口是否漏加了 `/v1`。
5. Docker 端口是不是你实际映射的端口。
6. 上游 API Key 或 Token Pool 是否可用。

## 5. 常见推荐配置

| 使用目标 | 推荐客户端地址 | 推荐端点转换器 |
|----------|----------------|----------------|
| Claude Code 直连 | `http://127.0.0.1:3021` | `claude` |
| Claude Code 通过 CC Switch | CC Switch 管理，Provider 指向 `http://127.0.0.1:3021` | `claude` |
| Codex CLI 直连 | `http://127.0.0.1:3021/v1` | `openai2` |
| Codex CLI 通过 CC Switch | CC Switch 管理，Provider 指向 `http://127.0.0.1:3021/v1` | `openai2` |
| Hermes Claude 模式 | `http://127.0.0.1:3021` | `claude` |
| Hermes OpenAI 模式 | `http://127.0.0.1:3021/v1` | `openai` 或 `openai2` |

## 主要能力

- 统一 API Provider：多个客户端共用一个本地入口。
- 多端点轮换与故障转移：上游失败后自动跳过并尝试备用端点。
- 多协议转换：支持 Claude、OpenAI Chat、OpenAI Responses、Gemini、DeepSeek、Kimi 等格式。
- Codex Token Pool：支持 token 导入、自动轮换、刷新、失效隔离、额度快照和用量统计。
- 模型聚合：提供 `/v1/models`、`/v1/models/{id}`、`/models`、`/api/tags` 等兼容接口。
- Web UI：管理端点、Token Pool、统计、备份、故障转移策略和运行状态。
- 备份恢复：支持 WebDAV、本地备份和 S3 兼容存储。

## 部署到远程服务器时的安全建议

如果你把 ccNexus 部署到公网服务器，不建议直接裸露端口。至少做到：

- 设置 `CCNEXUS_BASIC_AUTH_USERNAME` 和 `CCNEXUS_BASIC_AUTH_PASSWORD`。
- 用防火墙只允许自己的 IP 访问。
- 通过 Nginx、Caddy 或 Cloudflare Tunnel 提供 HTTPS。
- 不要把上游 API Key、token JSON、数据库文件提交到 Git。
- 数据目录建议持久化备份，例如 Docker 的 `/data` 卷。

## 运行模式

| 模式 | 入口 | 适合场景 |
|------|------|----------|
| Server | `cmd/server` | 本机、远程服务器、NAS、Docker、无头 API 代理 |
| Web UI | `cmd/server/webui` | 浏览器中管理端点、Token Pool、统计、备份和故障转移策略 |
| Desktop source | `cmd/desktop` | 历史 Wails 源码保留；不是当前维护分支的推荐安装方式 |

server 模式支持这些常用环境变量：

| 变量 | 作用 |
|------|------|
| `CCNEXUS_PORT` | 服务监听端口 |
| `CCNEXUS_LOG_LEVEL` | 日志级别 |
| `CCNEXUS_DATA_DIR` | 数据目录 |
| `CCNEXUS_DB_PATH` | SQLite 数据库路径 |
| `CCNEXUS_BASIC_AUTH_ENABLED` | 是否启用 Basic Auth |
| `CCNEXUS_BASIC_AUTH_USERNAME` | Basic Auth 用户名 |
| `CCNEXUS_BASIC_AUTH_PASSWORD` | Basic Auth 密码 |

## 与初代版本的差异

当前 fork 延续了 [lich0821/ccNexus](https://github.com/lich0821/ccNexus) 初代项目“本地统一代理入口”的核心思路，但把重点从简单轮换扩展到长期运行、多端点并发和复杂上游错误恢复。

| 维度 | 初代版本优势 | 当前 fork 增强 |
|------|--------------|----------------|
| 故障切换模型 | 失败后全局轮换端点，行为直观 | 单次请求内 fallback，不轻易改变全局默认端点，并发请求互不污染 |
| 错误识别 | 策略简单，维护成本低 | 区分额度耗尽、限流、上游 5xx、网络异常、API Key 失效、客户端 invalid request 等场景 |
| 端点恢复 | 没有额外状态，结果容易预测 | 失败端点进入可配置冷却，恢复后可自动返回或降优先级 |
| 流式稳定性 | 实现简洁，接近传统 HTTP 代理行为 | 支持 SSE heartbeat、上游强制流式、流式错误分类和空输出检测 |
| 运维可见性 | 基础日志和统计 | Request ID、重试次数、失败原因、端点运行态与凭证级用量/额度快照 |

## 文档

- [详细配置](docs/configuration.md)
- [开发指南](docs/development.md)
- [常见问题](docs/FAQ.md)
- [Docker 说明](docs/README_DOCKER.md)
- [CC Switch 官网](https://cc-switch.cc/)

## 许可证

[MIT](LICENSE)
