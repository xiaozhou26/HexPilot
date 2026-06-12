# HexPilot API Proxy

一个 Go + Gin 的 API 转接服务，让不支持工具调用（tool calling）和深度思考的逆向 API 能够支持这些高级功能。

## 架构原理

```

客户端 (Claude/ChatGPT等)
    │
    ▼
┌─────────────────────────┐
│   /v1/responses 接口     │  ← OpenAI Responses API 兼容
│   (Gin HTTP Server)      │
└─────────┬───────────────┘
          │
    ┌─────┴─────┐
    ▼           ▼
┌────────┐  ┌──────────────┐
│ ReAct  │  │ Plan &       │  ← 两种 AI 推理模式
│ 模式    │  │ Execute 模式  │
└───┬────┘  └──────┬───────┘
    │              │
    ▼              ▼
┌─────────────────────────┐
│   Tool Registry          │  ← 工具注册与执行
│   (search, run_code...)  │
└─────────────────────────┘
    │
    ▼
┌─────────────────────────┐
│   Proxy Layer            │  ← 转发到上游逆向 API
│   (/v1/chat/completions) │
└─────────────────────────┘
    │
    ▼
上游逆向 API (不支持 tool calling)
```

## 两种模式

### 1. ReAct 模式 (Reason + Act)
- AI 逐步推理 → 决定调用工具 → 执行工具 → 观察结果 → 继续推理
- 循环直到 AI 认为任务完成
- 适合需要多步交互的复杂任务

### 2. Plan & Execute 模式
- 类似 Claude Code 的 plan 模式
- 第一阶段：AI 先制定完整计划
- 第二阶段：按计划逐步执行
- 第三阶段：基于执行结果生成最终回答
- 适合需要系统性规划的任务

## 快速开始

### 环境变量

```bash
# 服务端口
export SERVER_PORT=8080

# 上游逆向 API 地址
export UPSTREAM_API=https://your-reverse-api.com

# 上游 API token（如果有）
export UPSTREAM_TOKEN=your-token-here

# 默认模式：react 或 plan_execute
export DEFAULT_MODE=react
```

### 运行

```bash
go mod tidy
go run main.go
```

## API 使用

### POST /v1/responses

```json
{
  "model": "your-model",
  "input": "帮我搜索最新的 AI 新闻",
  "mode": "react",
  "thinking": {
    "type": "enabled",
    "budget_tokens": 4000
  },
  "instructions": "你是一个助手",
  "tools": [
    {
      "type": "function",
      "name": "search",
      "description": "搜索信息"
    }
  ]
}
```

### 模式选择

- 请求中设置 `"mode": "react"` 使用 ReAct 模式
- 请求中设置 `"mode": "plan_execute"` 使用 Plan & Execute 模式
- 不设置则使用环境变量 `DEFAULT_MODE` 指定的默认模式

### POST /v1/chat/completions

标准 OpenAI 聊天接口，直接转发到上游 API。

## 添加自定义工具

在 `internal/tools/tools.go` 的 `RegisterBuiltins` 函数中添加：

```go
registry.Register("my_tool", "工具描述", func(ctx context.Context, args map[string]interface{}) (string, error) {
    // 实现你的工具逻辑
    return "结果", nil
})
```

## Codex Responses Provider

HexPilot exposes an OpenAI-compatible Responses endpoint at:

```text
http://localhost:8080/v1/responses
```

Use this in your user-level Codex config, for example `C:\Users\<you>\.codex\config.toml`:

```toml
model = "deepseek/deepseek-chat-v3-0324"
model_provider = "hexpilot"

[model_providers.hexpilot]
name = "HexPilot"
base_url = "http://localhost:8080/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
```

HexPilot does not require an API key by default. If Codex requires the env var to exist, set a dummy value:

```powershell
$env:OPENAI_API_KEY = "dummy"
```

Smoke test:

```powershell
$body = '{"model":"deepseek/deepseek-chat-v3-0324","input":"ping","store":false}'
curl.exe -sS -X POST http://localhost:8080/v1/responses -H "Content-Type: application/json" -d $body
```

Streaming smoke test:

```powershell
$body = '{"model":"deepseek/deepseek-chat-v3-0324","input":"ping","stream":true,"store":false}'
curl.exe -N -X POST http://localhost:8080/v1/responses -H "Content-Type: application/json" -d $body
```
