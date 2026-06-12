# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

HexPilot is a Go + Gin API proxy that adds tool-calling and deep-thinking capabilities to reverse-engineered upstream APIs that don't support them natively. Clients (Codex, Claude-style) hit HexPilot, HexPilot translates the request, drives a ReAct or Plan-and-Execute loop against the upstream `/v1/chat/completions` endpoint, executes local tools, and returns Responses / Messages / Chat-Completions-shaped responses.

Module path: `github.com/hexpilot/api-proxy`. Go 1.25. Only direct dependency: `github.com/gin-gonic/gin v1.12.0`.

## Build & Run

```bash
go mod tidy            # resolve deps
go build -o hexpilot.exe .   # build server
go run main.go         # run server (loads .env automatically)
```

Config lives in `.env` (template: `.env.example`). Key vars: `SERVER_PORT`, `UPSTREAM_API`, `UPSTREAM_TOKEN`, `DEFAULT_MODEL`, `UPSTREAM_NATIVE_TOOLS`, `DEFAULT_MODE` (`react` | `plan_execute`). `internal/config/config.go` loads `.env` then `.env.local`.

## Tests

```bash
go test ./...                                  # all packages
go test ./internal/proxy/...                   # one package
go test -run TestParseSSEToResponse ./...      # one test
go test -v ./internal/handler/...              # verbose for the handler package
```

Tests are colocated (`*_test.go`) next to the code they cover. Notable files:
- `internal/proxy/proxy_test.go` — SSE parsing, tool-call chunk merging, text-to-chat-completion wrapping
- `internal/handler/handler_test.go` — response-context merging, chat-completions stream + tool adaptation via `httptest`
- `internal/mode/react/react_test.go` — ReAct engine behavior

### What to test (from `gen-test` skill)

- **ReAct** — single tool call, multi-call loop hitting `chatMaxToolCalls`, no-tool direct response, SSE event sequence
- **Plan & Execute** — full three-phase flow, plan parse error recovery, tool execution failure, `maxPlanSteps` cap
- **Proxy** — non-stream forwarding, SSE stream parsing & merging, HTML-response error path, `UPSTREAM_TOKEN` propagation
- **Handler** — `POST /v1/responses` (sync + stream), `POST /v1/chat/completions` with and without tools, `GET /health`, multi-format compat (Anthropic / OpenAI)

## Layout

```
main.go                              # Gin server bootstrap, route registration
internal/config/                     # env loading
internal/model/                      # request/response types (Responses API shape)
internal/proxy/                      # upstream HTTP client + SSE parsing
internal/tools/                      # ToolRegistry + RegisterBuiltins
internal/mode/react/                 # ReAct engine
internal/mode/planexecute/           # Plan & Execute engine
internal/handler/                    # HTTP handlers, route wiring, format adapters
```

## Architecture

### Request flow

```
Client → Gin router (internal/handler/handler.go)
  → /v1/responses         HandleResponses  (OpenAI Responses API, full feature set)
  → /v1/messages          HandleMessages   (Anthropic Messages API → internal)
  → /v1/chat/completions  HandleChatCompletions  (OpenAI Chat API)
  → /v1/responses/:id     GetResponse      (in-memory response store)
  → /v1/models, /health
```

`Handler` (in `internal/handler/handler.go`) owns the `proxy.Proxy`, `tools.ToolRegistry`, both engines, and an in-memory `responseStore` / `responses` map guarded by `storeMu`. `previous_response_id` is resolved from this store and merged via `mergeResponseContext`.

### Mode selection

`Handler.resolveMode(req)` picks `react` or `plan_execute`. Resolution order: request `mode` field → `cfg.DefaultMode` → `"react"`. Both engines share the same `proxy.Proxy` and `tools.ToolRegistry`; they differ in prompt strategy and loop shape.

### Engine interface (for adding new modes)

Defined by the `new-mode` skill, every mode package under `internal/mode/<name>/` must implement:

```go
type Engine interface {
    Process(ctx context.Context, req *model.ResponsesRequest) (*model.ResponsesResponse, error)
    ProcessStream(ctx context.Context, req *model.ResponsesRequest, onEvent func(*model.SSEEvent) error) error
}
```

`Handler.New` is the single wiring point — instantiate the engine and stash it on the `Handler`. The handler's stream paths expect to be able to call `ProcessStream` for streaming responses; the sync paths use `Process`. Existing `react` and `planexecute` packages are the reference implementations. After adding a mode, also extend `Handler.resolveMode` and the `Handler.ListModels` / `HealthCheck` "active mode" logic.

### ReAct engine (`internal/mode/react/react.go`)

Iterates up to `maxToolCalls = 10`. Per iteration:
1. Build upstream request with current messages.
2. If `cfg.UpstreamNativeTools`, pass OpenAI `tools`/`tool_choice` on iteration 0 only.
3. Call `proxy.ForwardRequest` (forces `stream:false`).
4. Try to extract tool calls in priority order:
   - `message.tool_calls` → convert to Responses output items.
   - `<action>{...}</action>` tag in content → parse as function call (this is the path used when the upstream has no native tool support).
   - `<final_answer>...</final_answer>` → final text.
5. Empty content triggers an `emptyResponseRetryPrompt` once before bailing.

Two prompt modes: `thinkingModePrompt` (wraps reasoning in `<thought>` tags) when `req.IsThinkingEnabled()` is true, otherwise `simpleModePrompt`. Triggered by `reasoning.effort ∈ {high, extra, max, maximum}`, `thinking.type=enabled`, or top-level `effort`.

### Plan & Execute engine (`internal/mode/planexecute/plan_execute.go`)

Three phases via three separate upstream calls:
1. `generatePlan` — produce JSON array of `{step, action, tool, input}`.
2. `executePlan` — for each step that names a tool, execute it locally; collect `executionResults`. Cap `maxPlanSteps = 20`.
3. `generateFinalResponse` — synthesize answer from plan + results.

### Proxy (`internal/proxy/proxy.go`)

`ForwardRequest` and `ForwardRequestStream` POST to `cfg.UpstreamAPI + "/v1/chat/completions"`, attaching `Authorization: Bearer <UPSTREAM_TOKEN>` if set. The client always asks for non-stream from the upstream; the outer SSE to the client is rebuilt by the handler.

The proxy is tolerant of upstream quirks:
- `isSSE` accepts `text/event-stream` content type, leading `data:`/`event:`, or embedded `\ndata:`.
- `parseSSEToResponse` concatenates `delta.content`, merges `delta.tool_calls` (handling `function.arguments` that arrives as a string across chunks), and synthesizes a chat-completion JSON. Falls back to wrapping each non-JSON chunk as a chat message via `wrapTextAsChatCompletion`.
- `looksLikeHTML` and `checkEmptyResponse` produce helpful error messages when the upstream returns an HTML page or empty choices — common when `UPSTREAM_API` points to a web UI or an adapter that's misrouted.

### Tool system (`internal/tools/tools.go`)

`ToolRegistry` is a `map[string]*RegisteredTool`. `Handler.New` constructs one and calls `tools.RegisterBuiltins(registry)` which currently registers three stubs: `search`, `run_code`, `read_file` — these are placeholders; real behavior must be added. Both engines pull tools from the same registry; the ReAct engine also accepts tags in content and is what gets used when the upstream has no native tool support.

### Chat completions adapter (`internal/handler/chat_completions.go`)

`ChatCompletionsEngine.Process` implements a standard OpenAI tool-calling loop (up to `chatMaxToolCalls = 10`) for upstreams that DO support native `tools` — passed through on iteration 0. When `cfg.UpstreamNativeTools == false` (the common case), the handler converts the request to a `ResponsesRequest` and routes it through the ReAct engine instead, then converts the result back via `ToChatCompletionResponse`.

`HandleChatCompletions` further distinguishes streaming vs non-streaming, and `streamChatCompletionResult` rebuilds OpenAI-style SSE chunks (including indexed `tool_calls` deltas and a final `finish_reason`).

### Anthropic adapter

`HandleMessages` accepts the Anthropic Messages API, calls `anthropicToInternal` to produce a `ResponsesRequest`, and routes through the same engines. Streaming path is `handleAnthropicStream`.

### Response storage

In-memory only, guarded by `sync.RWMutex` (`storeMu`). `storeResponseContext` is called on non-streaming `/v1/responses` completion. `GET /v1/responses/:id` reads from it. The store also backs `previous_response_id` chaining. There is no persistence — restart loses state.

## Conventions / gotchas

- `UPSTREAM_NATIVE_TOOLS=false` (default) means the upstream cannot do tool calls itself. HexPilot emulates them by parsing `<action>` tags out of the upstream's text response and executing tools locally. Flip to `true` only if the upstream genuinely returns OpenAI-shaped `tool_calls`.
- `cfg.DefaultModel` is used when the request omits `model`, and is also one of the IDs advertised by `/v1/models`. Update `.env.example` and the hard-coded list in `Handler.ListModels` together.
- The handler accepts a wide superset of fields (chat-completions migration: `max_tokens`, `response_format`, `messages`) and normalizes everything to `model.ResponsesRequest`. Look at `Handler.HandleResponses` before adding new fields.
- All upstream requests are forced to `stream:false` by the proxy. The outer streaming surface (SSE on `/v1/chat/completions` and `/v1/responses`) is reconstructed from the buffered response.
- `.env` is gitignored; use `.env.example` as the template. `.env.local` is loaded after `.env` and can override.
- The prebuilt `hexpilot.exe` and `.atomcode/` are gitignored. Don't commit them.

## Security notes (from `security-reviewer` skill)

These are the live concerns this codebase has — verify before changing related code:

- **Token handling** — `UPSTREAM_TOKEN` must never be logged. The proxy sets `Authorization: Bearer <token>` on upstream requests; check no `log.Printf` line in `internal/proxy/proxy.go` or `internal/handler/handler.go` prints the raw token or full `Authorization` header.
- **SSRF** — `UPSTREAM_API` is taken straight from env with no allowlist/validation. If you add user-controlled inputs that influence the upstream URL, gate them. The current code is safe because the only consumer of `UPSTREAM_API` is the operator's `.env`.
- **HTML-detection error path** — `proxy.looksLikeHTML` already returns a clear error when the upstream returns an HTML page. Don't suppress that branch; it's how a misconfigured `UPSTREAM_API` is diagnosed.
- **Tool argument validation** — tools receive `args map[string]interface{}` from JSON in the upstream response and should validate types/ranges before using. The current `RegisterBuiltins` stubs are placeholders — real tool implementations must validate.
- **SSE injection** — when concatenating `delta.content` from upstream chunks into a single response (`parseSSEToResponse`), the bytes flow into the outer SSE the client sees. Don't echo raw `data:` lines from untrusted sources; the current code parses each chunk as JSON before writing, which is the safe path.

## Project skills

`.claude/skills/` contains project-specific skills (also mirrored under `.atomcode/skills/`):
- `api-documenter` — generate / update API docs and check drift against code
- `gen-test` — guided test generation per layer
- `new-mode` — scaffolding for adding a new AI inference mode (defines the `Engine` interface)
- `security-reviewer` — security checklist used during code review

Invoke with `/<skill-name>` (e.g. `/new-mode plan_execute_v2`, `/gen-test handler`).
