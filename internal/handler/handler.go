package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hexpilot/api-proxy/internal/config"
	"github.com/hexpilot/api-proxy/internal/mode/planexecute"
	"github.com/hexpilot/api-proxy/internal/mode/react"
	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

type Handler struct {
	config         *config.Config
	proxy          *proxy.Proxy
	toolRegistry   *tools.ToolRegistry
	reactEngine    *react.ReActEngine
	planExecEngine *planexecute.PlanExecuteEngine
	chatEngine     *ChatCompletionsEngine
	responseStore  map[string][]interface{}
	responses      map[string]*model.ResponsesResponse
	storeMu        sync.RWMutex
}

func New(cfg *config.Config) *Handler {
	p := proxy.New(cfg)
	registry := tools.NewRegistry()
	tools.RegisterBuiltins(registry)

	return &Handler{
		config:         cfg,
		proxy:          p,
		toolRegistry:   registry,
		reactEngine:    react.NewEngine(p, registry),
		planExecEngine: planexecute.NewEngine(p, registry),
		chatEngine:     NewChatEngine(p, registry),
		responseStore:  make(map[string][]interface{}),
		responses:      make(map[string]*model.ResponsesResponse),
	}
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// OpenAI Responses API 兼容接口
	r.POST("/v1/responses", h.HandleResponses)
	r.GET("/v1/responses/:id", h.GetResponse)

	// Anthropic Messages API 兼容接口
	r.POST("/v1/messages", h.HandleMessages)

	// OpenAI 标准聊天完成接口
	r.POST("/v1/chat/completions", h.HandleChatCompletions)

	// 健康检查
	r.GET("/health", h.HealthCheck)

	// 列出支持的模型（欺骗客户端）
	r.GET("/v1/models", h.ListModels)
}

// HandleResponses 处理 OpenAI /v1/responses 请求
func (h *Handler) HandleResponses(c *gin.Context) {
	// 先读取原始 body 到 map，确保 interface{} 字段正确解析
	var rawBody map[string]interface{}
	if err := c.ShouldBindJSON(&rawBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	log.Printf("[HandleResponses] rawBody keys=%v, input=%v", mapKeys(rawBody), rawBody["input"])

	// 从 rawBody 构建 ResponsesRequest
	req := &model.ResponsesRequest{}
	if v, ok := rawBody["model"].(string); ok {
		req.Model = v
	}
	if req.Model == "" {
		req.Model = h.config.DefaultModel
	}
	req.ResponseID = newResponseID()
	if v, ok := rawBody["input"]; ok {
		req.Input = v
	}
	// 兼容 chat/completions 格式：用 messages 字段作为 input
	if req.Input == nil {
		if msgs, ok := rawBody["messages"].([]interface{}); ok && len(msgs) > 0 {
			req.Input = msgs
		}
	}
	if v, ok := rawBody["stream"].(bool); ok {
		req.Stream = v
	}
	if v, ok := rawBody["max_output_tokens"].(float64); ok {
		req.MaxOutput = int(v)
	}
	// Chat Completions migration compatibility.
	if req.MaxOutput == 0 {
		if v, ok := rawBody["max_tokens"].(float64); ok {
			req.MaxOutput = int(v)
		}
	}
	if v, ok := rawBody["temperature"].(float64); ok {
		req.Temperature = v
	}
	if v, ok := rawBody["top_p"].(float64); ok {
		req.TopP = v
	}
	if v, ok := rawBody["mode"].(string); ok {
		req.Mode = v
	}
	if v, ok := rawBody["instructions"].(string); ok {
		req.Instructions = v
	}
	if v, ok := rawBody["prompt"].(string); ok {
		req.Prompt = v
	}
	if v, ok := rawBody["system"].(string); ok {
		req.System = v
	}
	if v, ok := rawBody["effort"].(string); ok {
		req.Effort = v
	}
	if v, ok := rawBody["reasoning_effort"].(string); ok {
		req.Effort = v
	}
	if v, ok := rawBody["verbosity"].(string); ok {
		req.Verbosity = v
	}
	// reasoning
	if r, ok := rawBody["reasoning"].(map[string]interface{}); ok {
		req.Reasoning = &model.ReasoningConfig{}
		if e, ok := r["effort"].(string); ok {
			req.Reasoning.Effort = e
		}
		if s, ok := r["summary"].(string); ok {
			req.Reasoning.Summary = s
		}
	}
	// thinking
	if t, ok := rawBody["thinking"].(map[string]interface{}); ok {
		req.Thinking = &model.ThinkingConfig{}
		if tt, ok := t["type"].(string); ok {
			req.Thinking.Type = tt
		}
		if b, ok := t["budget_tokens"].(float64); ok {
			req.Thinking.BudgetTokens = int(b)
		}
	}
	// text (支持 verbosity 和 format 两种子字段)
	if tx, ok := rawBody["text"].(map[string]interface{}); ok {
		req.Text = &model.TextConfig{}
		if v, ok := tx["verbosity"].(string); ok {
			req.Text.Verbosity = v
		}
		if f, ok := tx["format"].(map[string]interface{}); ok {
			fmtType, _ := f["type"].(string)
			if fmtType == "json_schema" {
				name, _ := f["name"].(string)
				if name == "" {
					name = "Output"
				}
				strict := false
				if s, ok := f["strict"].(bool); ok {
					strict = s
				}
				req.Text.Format = &model.TextFormat{
					Type:   "json_schema",
					Name:   name,
					Schema: f["schema"],
					Strict: &strict,
				}
			} else {
				req.Text.Format = &model.TextFormat{Type: fmtType}
			}
		}
	}
	// tools（标准格式 + function 包装兼容）
	if toolsArr, ok := rawBody["tools"].([]interface{}); ok {
		req.Tools = parseResponseTools(toolsArr)
	}
	if v, ok := rawBody["tool_choice"]; ok {
		req.ToolChoice = v
	}
	if v, ok := rawBody["allowed_tools"].([]interface{}); ok {
		for _, at := range v {
			if s, ok := at.(string); ok {
				req.AllowedTools = append(req.AllowedTools, s)
			}
		}
	}
	// truncation & previous_response_id
	if v, ok := rawBody["truncation"].(string); ok {
		req.Truncation = v
	}
	if v, ok := rawBody["previous_response_id"].(string); ok {
		req.PreviousRespID = v
	}
	// store (默认 true 匹配 OpenAI 行为)
	if v, ok := rawBody["store"].(bool); ok {
		req.Store = v
	} else {
		req.Store = true
	}
	// parallel_tool_calls (默认 true)
	if v, ok := rawBody["parallel_tool_calls"].(bool); ok {
		req.ParallelToolCalls = v
	} else {
		req.ParallelToolCalls = true
	}
	// user
	if v, ok := rawBody["user"].(string); ok {
		req.User = v
	}
	// metadata
	if v, ok := rawBody["metadata"].(map[string]interface{}); ok {
		meta := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				meta[k] = s
			}
		}
		req.Metadata = meta
	}
	// include
	if v, ok := rawBody["include"].([]interface{}); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				req.Include = append(req.Include, s)
			}
		}
	}
	// stream_options
	if so, ok := rawBody["stream_options"].(map[string]interface{}); ok {
		req.StreamOptions = &model.StreamOptions{}
		if inc, ok := so["include_usage"].(bool); ok {
			req.StreamOptions.IncludeUsage = inc
		}
	}

	// response_format -> text.format 向后兼容（Chat Completions 迁移）
	if rf, ok := rawBody["response_format"].(map[string]interface{}); ok && req.Text == nil {
		req.Text = &model.TextConfig{}
		rfType, _ := rf["type"].(string)
		switch rfType {
		case "json_object":
			req.Text.Format = &model.TextFormat{Type: "json_object"}
		case "json_schema":
			if js, ok := rf["json_schema"].(map[string]interface{}); ok {
				name, _ := js["name"].(string)
				if name == "" {
					name = "Output"
				}
				strict := false
				if s, ok := js["strict"].(bool); ok {
					strict = s
				}
				req.Text.Format = &model.TextFormat{
					Type:   "json_schema",
					Name:   name,
					Schema: js["schema"],
					Strict: &strict,
				}
			}
		default:
			req.Text.Format = &model.TextFormat{Type: "text"}
		}
	}
	// 确保 text.format 有默认值
	if req.Text != nil && req.Text.Format == nil {
		req.Text.Format = &model.TextFormat{Type: "text"}
	}

	req.RawBody = rawBody
	req.UpstreamNativeTools = h.config.UpstreamNativeTools

	// 规范化思考配置（统一映射）
	h.normalizeReasoning(req)

	log.Printf("[/v1/responses] model=%s, mode=%s, effort=%s, stream=%v",
		req.Model, h.resolveMode(req), h.getEffectiveEffort(req), req.Stream)

	mode := h.resolveMode(req)

	// 校验上游配置
	if h.config.UpstreamAPI == "" || h.config.UpstreamAPI == "https://api.example.com" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "UPSTREAM_API not configured. Set UPSTREAM_API environment variable to your reverse-engineered API endpoint.",
		})
		return
	}

	if req.PreviousRespID != "" {
		previous, ok := h.loadResponseContext(req.PreviousRespID)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("previous_response_id not found: %s", req.PreviousRespID),
			})
			return
		}
		req.Input = mergeResponseContext(previous, req.Input)
	}

	if req.Stream {
		h.handleResponsesStream(c, req, mode)
	} else {
		h.handleResponsesSync(c, req, mode)
	}
}

// GetResponse returns an in-memory stored Responses object.
func (h *Handler) GetResponse(c *gin.Context) {
	responseID := c.Param("id")

	h.storeMu.RLock()
	resp, ok := h.responses[responseID]
	h.storeMu.RUnlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("response not found: %s", responseID),
		})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// HandleMessages 处理 Anthropic /v1/messages 请求
// 将 Anthropic 格式转换为内部格式，然后通过 ReAct/Plan 引擎处理
func (h *Handler) HandleMessages(c *gin.Context) {
	var rawReq map[string]interface{}
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Accept Claude Code headers silently -- not forwarded upstream.
	c.Request.Header.Del("x-api-key")
	c.Request.Header.Del("anthropic-version")
	c.Request.Header.Del("anthropic-dangerous-direct-browser-access")

	// 转换为 ResponsesRequest
	req := h.anthropicToInternal(rawReq)

	log.Printf("[/v1/messages] model=%s, mode=%s, thinking=%v, effort=%s",
		req.Model, h.resolveMode(&req), req.Thinking != nil, h.getEffectiveEffort(&req))

	mode := h.resolveMode(&req)

	if req.Stream {
		h.handleAnthropicStream(c, &req, mode)
	} else {
		h.handleAnthropicSync(c, &req, mode)
	}
}

// HandleChatCompletions 处理标准聊天完成请求（支持工具调用循环）
func (h *Handler) HandleChatCompletions(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if getBool(req, "stream", false) {
		h.handleChatCompletionsStream(c, req)
		return
	}

	result, err := h.processChatCompletions(c, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

func (h *Handler) processChatCompletions(c *gin.Context, req map[string]interface{}) (map[string]interface{}, error) {
	// 检查是否有 tools 定义
	if tools, ok := req["tools"].([]interface{}); ok && len(tools) > 0 {
		if !h.config.UpstreamNativeTools {
			responsesReq := h.chatCompletionsToResponsesRequest(req)
			responsesReq.ResponseID = newResponseID()
			result, err := h.reactEngine.Process(c.Request.Context(), responsesReq)
			if err != nil {
				return nil, err
			}
			h.enrichResponse(result, responsesReq)
			return ToChatCompletionResponse(result), nil
		}

		// 有工具，使用 ReAct 循环处理
		result, err := h.chatEngine.Process(c.Request.Context(), req)
		if err != nil {
			return nil, err
		}
		return result, nil
	}

	// 没有工具，直接转发到上游
	result, err := h.proxy.ForwardRequest(req)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (h *Handler) chatCompletionsToResponsesRequest(req map[string]interface{}) *model.ResponsesRequest {
	responsesReq := &model.ResponsesRequest{
		Model:               getString(req, "model", h.config.DefaultModel),
		Input:               chatMessagesToResponsesInput(extractMessages(req)),
		Stream:              getBool(req, "stream", false),
		MaxOutput:           getInt(req, "max_tokens", 0),
		Temperature:         getFloat(req, "temperature", 0),
		TopP:                getFloat(req, "top_p", 0),
		TopK:                getTopK(req),
		StopSequences:       getStringSlice(req, "stop_sequences"),
		ToolChoice:          req["tool_choice"],
		ParallelToolCalls:   getBool(req, "parallel_tool_calls", true),
		Store:               getBool(req, "store", false),
		UpstreamNativeTools: h.config.UpstreamNativeTools,
		RawBody:             req,
	}
	if responsesReq.Model == "" {
		responsesReq.Model = h.config.DefaultModel
	}
	if toolsArr, ok := req["tools"].([]interface{}); ok {
		responsesReq.Tools = parseResponseTools(toolsArr)
	}
	h.normalizeReasoning(responsesReq)
	return responsesReq
}

func (h *Handler) handleChatCompletionsStream(c *gin.Context, req map[string]interface{}) {
	h.setupSSEHeaders(c)

	result, err := h.processChatCompletions(c, req)
	if err != nil {
		h.sendRawSSE(c, "error", gin.H{"message": err.Error()})
		c.Writer.Flush()
		return
	}

	h.streamChatCompletionResult(c, result)
}

func (h *Handler) streamChatCompletionResult(c *gin.Context, result map[string]interface{}) {
	id := getString(result, "id", newChatCompletionID())
	modelName := getString(result, "model", h.config.DefaultModel)
	created := time.Now().Unix()

	message, finishReason := chatCompletionMessageAndFinishReason(result)
	if finishReason == "" {
		finishReason = "stop"
	}

	h.sendChatCompletionChunk(c, id, modelName, created, gin.H{"role": "assistant"}, nil)

	if content, ok := message["content"].(string); ok {
		for _, delta := range chunkString(content, 256) {
			h.sendChatCompletionChunk(c, id, modelName, created, gin.H{"content": delta}, nil)
		}
	}

	if toolCalls, ok := message["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
		h.sendChatCompletionChunk(c, id, modelName, created, gin.H{"tool_calls": indexedToolCallDeltas(toolCalls)}, nil)
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	h.sendChatCompletionChunk(c, id, modelName, created, gin.H{}, &finishReason)
	fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()
}

func (h *Handler) sendChatCompletionChunk(c *gin.Context, id, modelName string, created int64, delta gin.H, finishReason *string) {
	chunk := gin.H{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []gin.H{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}

func chatCompletionMessageAndFinishReason(result map[string]interface{}) (map[string]interface{}, string) {
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return map[string]interface{}{"role": "assistant", "content": ""}, "stop"
	}
	first, ok := choices[0].(map[string]interface{})
	if !ok {
		return map[string]interface{}{"role": "assistant", "content": ""}, "stop"
	}
	finishReason, _ := first["finish_reason"].(string)
	message, ok := first["message"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{"role": "assistant", "content": ""}, finishReason
	}
	return message, finishReason
}

func indexedToolCallDeltas(toolCalls []interface{}) []gin.H {
	out := make([]gin.H, 0, len(toolCalls))
	for i, raw := range toolCalls {
		toolCall, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		delta := gin.H{"index": i}
		if id, ok := toolCall["id"].(string); ok && id != "" {
			delta["id"] = id
		}
		if typ, ok := toolCall["type"].(string); ok && typ != "" {
			delta["type"] = typ
		} else {
			delta["type"] = "function"
		}
		if fn, ok := toolCall["function"].(map[string]interface{}); ok {
			delta["function"] = fn
		}
		out = append(out, delta)
	}
	return out
}

// HealthCheck 健康检查
func (h *Handler) HealthCheck(c *gin.Context) {
	// 判断当前活跃模式
	activeMode := h.config.DefaultMode
	if activeMode != "react" && activeMode != "plan_execute" {
		activeMode = "react"
	}
	c.JSON(http.StatusOK, gin.H{
		"status":                "ok",
		"time":                  time.Now().Format(time.RFC3339),
		"mode":                  activeMode,
		"upstream":              h.config.UpstreamAPI,
		"upstream_native_tools": h.config.UpstreamNativeTools,
		"endpoints": []string{
			"POST /v1/responses",
			"GET  /v1/responses/:id",
			"POST /v1/messages",
			"POST /v1/chat/completions",
			"GET  /v1/models",
			"GET  /health",
		},
	})
}

// ListModels 列出模型（欺骗客户端）
func (h *Handler) ListModels(c *gin.Context) {
	modelIDs := uniqueStrings([]string{
		h.config.DefaultModel,
		"deepseek/deepseek-chat-v3-0324",
		"gpt-5",
		"hexpilot-tool-use-v1",
		"hexpilot-reasoning-v1",
	})
	data := make([]gin.H, 0, len(modelIDs))
	for _, id := range modelIDs {
		if id == "" {
			continue
		}
		data = append(data, gin.H{
			"id":       id,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "hexpilot",
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}

// ===== 内部处理 =====

func (h *Handler) handleResponsesSync(c *gin.Context, req *model.ResponsesRequest, mode string) {
	result, err := h.executeMode(c, req, mode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 用请求参数填充标准响应字段
	h.enrichResponse(result, req)
	h.storeResponseContext(result.ID, req, result)
	c.JSON(http.StatusOK, result)
}

func (h *Handler) handleResponsesStream(c *gin.Context, req *model.ResponsesRequest, mode string) {
	h.setupSSEHeaders(c)
	seq := 0

	inProgress := h.newResponseShell(req, "in_progress", nil)
	h.sendResponseStreamEvent(c, "response.created", gin.H{"response": inProgress}, &seq)
	h.sendResponseStreamEvent(c, "response.in_progress", gin.H{"response": inProgress}, &seq)

	result, err := h.executeMode(c, req, mode)
	if err != nil {
		h.sendResponseStreamEvent(c, "error", gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": err.Error(),
			},
		}, &seq)
		return
	}

	h.enrichResponse(result, req)
	h.storeResponseContext(result.ID, req, result)
	h.emitResponseOutputEvents(c, result, &seq)
	h.sendResponseStreamEvent(c, "response.completed", gin.H{"response": result}, &seq)
}

func (h *Handler) handleAnthropicSync(c *gin.Context, req *model.ResponsesRequest, mode string) {
	result, err := h.executeMode(c, req, mode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.enrichResponse(result, req)
	h.storeResponseContext(result.ID, req, result)

	// 转换为 Anthropic 格式响应
	c.JSON(http.StatusOK, h.toAnthropicResponse(result))
}

func (h *Handler) handleAnthropicStream(c *gin.Context, req *model.ResponsesRequest, mode string) {
	h.setupSSEHeaders(c)

	messageID := newMessageID()
	modelName := req.Model
	if modelName == "" {
		modelName = h.config.DefaultModel
	}

	var toolCallsDetected []map[string]interface{}
	textStarted := false
	blockIndex := 0

	// 1. message_start
	h.sendRawSSE(c, "message_start", gin.H{
		"type": "message_start",
		"message": gin.H{
			"id":    messageID,
			"type":  "message",
			"role":  "assistant",
			"model": modelName,
			"usage": gin.H{"input_tokens": 0, "output_tokens": 1},
		},
	})

	// Ping keepalive for slow upstreams
	pingDone := make(chan struct{})
	pingTicker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-pingTicker.C:
				h.sendRawSSE(c, "ping", nil)
				c.Writer.Flush()
			case <-pingDone:
				return
			}
		}
	}()

	// 2. Stream through the engine
	streamErr := h.streamMode(c, req, mode,
		func(event *model.SSEEvent) error {
			switch event.Type {
			case "response.output_text.delta":
				delta, _ := event.Data.(map[string]interface{})
				text, _ := delta["delta"].(string)
				if !textStarted {
					h.sendRawSSE(c, "content_block_start", gin.H{
						"index": blockIndex,
						"content_block": gin.H{
							"type": "text",
							"text": "",
						},
					})
					textStarted = true
					c.Writer.Flush()
				}
				h.sendRawSSE(c, "content_block_delta", gin.H{
					"index": blockIndex,
					"delta": gin.H{"type": "text_delta", "text": text},
				})
				c.Writer.Flush()
			}
			return nil
		},
		func(tc map[string]interface{}) {
			toolCallsDetected = append(toolCallsDetected, tc)
		},
	)

	pingTicker.Stop()
	close(pingDone)

	// Emit error early if stream failed (before normal finalization)
	if streamErr != nil {
		if textStarted {
			h.sendRawSSE(c, "content_block_stop", gin.H{"index": blockIndex})
		}
		for i := range toolCallsDetected {
			h.sendRawSSE(c, "content_block_stop", gin.H{"index": blockIndex + 1 + i})
		}
		h.sendRawSSE(c, "message_delta", gin.H{
			"type": "message_delta",
			"delta": gin.H{
				"stop_reason":   "error",
				"stop_sequence": nil,
			},
			"usage": gin.H{"output_tokens": 0},
		})
		h.sendRawSSE(c, "error", gin.H{
			"type":    "upstream_error",
			"message": streamErr.Error(),
		})
		h.sendRawSSE(c, "message_stop", gin.H{"type": "message_stop"})
		c.Writer.Flush()
		return
	}

	// 3. Finalize text block (text is always at blockIndex=0)
	if textStarted {
		h.sendRawSSE(c, "content_block_stop", gin.H{"index": 0})
		blockIndex = 1
	}

	// 4. Emit tool_use blocks if any tool calls were detected
	stopReason := "end_turn"
	if len(toolCallsDetected) > 0 {
		stopReason = "tool_use"
		for _, tc := range toolCallsDetected {
			fn, _ := tc["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			argsJSON, _ := fn["arguments"].(string)
			if argsJSON == "" {
				argsJSON = "{}"
			}

			h.sendRawSSE(c, "content_block_start", gin.H{
				"index": blockIndex,
				"content_block": gin.H{
					"type": "tool_use",
					"id":   tc["id"],
					"name": name,
					"input": gin.H{},
				},
			})
			c.Writer.Flush()

			for _, chunk := range chunkString(argsJSON, 64) {
				h.sendRawSSE(c, "content_block_delta", gin.H{
					"index": blockIndex,
					"delta": gin.H{"type": "input_json_delta", "partial_json": chunk},
				})
				c.Writer.Flush()
			}

			h.sendRawSSE(c, "content_block_stop", gin.H{"index": blockIndex})
			c.Writer.Flush()
			blockIndex++
		}
	}

	// 5. message_delta with stop_reason
	h.sendRawSSE(c, "message_delta", gin.H{
		"type": "message_delta",
		"delta": gin.H{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": gin.H{"output_tokens": 0},
	})

	// 6. message_stop
	h.sendRawSSE(c, "message_stop", gin.H{"type": "message_stop"})
	c.Writer.Flush()
}

func (h *Handler) executeMode(c *gin.Context, req *model.ResponsesRequest, mode string) (*model.ResponsesResponse, error) {
	switch mode {
	case "plan_execute":
		return h.planExecEngine.Process(c.Request.Context(), req)
	case "react":
		fallthrough
	default:
		return h.reactEngine.Process(c.Request.Context(), req)
	}
}

func (h *Handler) streamMode(c *gin.Context, req *model.ResponsesRequest, mode string, onEvent func(*model.SSEEvent) error, onToolCall func(map[string]interface{})) error {
	switch mode {
	case "plan_execute":
		return h.streamPlanExecute(c, req, onEvent, nil)
	case "react":
		fallthrough
	default:
		return h.reactEngine.ProcessStream(c.Request.Context(), req, onEvent, onToolCall)
	}
}

func (h *Handler) streamPlanExecute(c *gin.Context, req *model.ResponsesRequest, onEvent func(*model.SSEEvent) error, _ func(map[string]interface{})) error {
	onEvent(&model.SSEEvent{Type: "plan.started", Data: gin.H{"message": "Generating plan..."}})

	result, err := h.planExecEngine.Process(c.Request.Context(), req)
	if err != nil {
		return err
	}

	for _, item := range result.Output {
		onEvent(&model.SSEEvent{Type: "response.output.added", Data: item})
	}

	return nil
}

func (h *Handler) newResponseShell(req *model.ResponsesRequest, status string, output []model.OutputItem) *model.ResponsesResponse {
	resp := &model.ResponsesResponse{
		ID:                req.ResponseID,
		Object:            "response",
		CreatedAt:         time.Now().Unix(),
		Status:            status,
		Output:            output,
		Model:             req.Model,
		ParallelToolCalls: req.ParallelToolCalls,
	}
	h.enrichResponse(resp, req)
	return resp
}

func (h *Handler) sendResponseStreamEvent(c *gin.Context, eventType string, data gin.H, seq *int) {
	if data == nil {
		data = gin.H{}
	}
	data["type"] = eventType
	data["sequence_number"] = *seq
	*seq = *seq + 1
	h.sendSSE(c, eventType, data)
	c.Writer.Flush()
}

func (h *Handler) emitResponseOutputEvents(c *gin.Context, resp *model.ResponsesResponse, seq *int) {
	for outputIndex, item := range resp.Output {
		addedItem := item
		if addedItem.Status == "completed" {
			addedItem.Status = "in_progress"
		}
		if addedItem.Status == "" {
			addedItem.Status = "in_progress"
		}
		if addedItem.Type == "message" {
			addedItem.Content = nil
		}
		if addedItem.Type == "function_call" {
			addedItem.Arguments = ""
		}

		h.sendResponseStreamEvent(c, "response.output_item.added", gin.H{
			"output_index": outputIndex,
			"item":         addedItem,
		}, seq)

		switch item.Type {
		case "message":
			h.emitMessageContentEvents(c, item, outputIndex, seq)
		case "function_call":
			h.emitFunctionCallEvents(c, item, outputIndex, seq)
		}

		h.sendResponseStreamEvent(c, "response.output_item.done", gin.H{
			"output_index": outputIndex,
			"item":         item,
		}, seq)
	}
}

func (h *Handler) emitMessageContentEvents(c *gin.Context, item model.OutputItem, outputIndex int, seq *int) {
	for contentIndex, block := range item.Content {
		text := contentBlockTextValue(block)
		h.sendResponseStreamEvent(c, "response.content_part.added", gin.H{
			"item_id":       item.ID,
			"output_index":  outputIndex,
			"content_index": contentIndex,
			"part":          contentPartPayload(""),
		}, seq)

		for _, delta := range chunkString(text, 256) {
			h.sendResponseStreamEvent(c, "response.output_text.delta", gin.H{
				"item_id":       item.ID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"delta":         delta,
				"logprobs":      []interface{}{},
			}, seq)
		}

		h.sendResponseStreamEvent(c, "response.output_text.done", gin.H{
			"item_id":       item.ID,
			"output_index":  outputIndex,
			"content_index": contentIndex,
			"text":          text,
			"logprobs":      []interface{}{},
		}, seq)
		h.sendResponseStreamEvent(c, "response.content_part.done", gin.H{
			"item_id":       item.ID,
			"output_index":  outputIndex,
			"content_index": contentIndex,
			"part":          contentPartPayload(text),
		}, seq)
	}
}

func (h *Handler) emitFunctionCallEvents(c *gin.Context, item model.OutputItem, outputIndex int, seq *int) {
	arguments := outputArgumentsString(item.Arguments)
	for _, delta := range chunkString(arguments, 256) {
		h.sendResponseStreamEvent(c, "response.function_call_arguments.delta", gin.H{
			"item_id":      item.ID,
			"output_index": outputIndex,
			"call_id":      item.CallID,
			"delta":        delta,
		}, seq)
	}
	h.sendResponseStreamEvent(c, "response.function_call_arguments.done", gin.H{
		"item_id":      item.ID,
		"output_index": outputIndex,
		"call_id":      item.CallID,
		"arguments":    arguments,
	}, seq)
}

// ===== 思考等级规范化 =====

// normalizeReasoning 统一处理所有平台的思考/推理配置
func (h *Handler) normalizeReasoning(req *model.ResponsesRequest) {
	// 优先级：reasoning.effort > thinking > effort > verbosity/text.verbosity

	// 1. 从 Anthropic thinking 推断 effort
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		effort := h.budgetTokensToEffort(req.Thinking.BudgetTokens)
		if req.Reasoning == nil {
			req.Reasoning = &model.ReasoningConfig{Effort: effort}
		}
	}

	// 2. 从 effort 字段推断
	if req.Effort != "" && req.Reasoning == nil {
		req.Reasoning = &model.ReasoningConfig{Effort: h.normalizeEffort(req.Effort)}
	}

	// 3. 从 verbosity 推断（旧版 OpenAI）
	if req.Verbosity != "" && req.Reasoning == nil {
		req.Reasoning = &model.ReasoningConfig{Effort: h.verbosityToEffort(req.Verbosity)}
	}

	// 4. 从 text.verbosity 推断
	if req.Text != nil && req.Text.Verbosity != "" && req.Reasoning == nil {
		req.Reasoning = &model.ReasoningConfig{Effort: h.verbosityToEffort(req.Text.Verbosity)}
	}

	// 5. 默认 medium
	if req.Reasoning == nil {
		req.Reasoning = &model.ReasoningConfig{Effort: "medium"}
	}
}

func (h *Handler) budgetTokensToEffort(tokens int) string {
	switch {
	case tokens <= 1000:
		return "low"
	case tokens <= 4000:
		return "medium"
	case tokens <= 16000:
		return "high"
	default:
		return "high"
	}
}

func (h *Handler) normalizeEffort(e string) string {
	switch strings.ToLower(e) {
	case "minimal", "min":
		return "minimal"
	case "low":
		return "low"
	case "medium", "med", "mid":
		return "medium"
	case "high":
		return "high"
	case "extra", "extended":
		return "high"
	case "max", "maximum":
		return "high"
	default:
		return "medium"
	}
}

func (h *Handler) verbosityToEffort(v string) string {
	switch strings.ToLower(v) {
	case "low", "brief", "concise":
		return "low"
	case "medium", "moderate":
		return "medium"
	case "high", "verbose", "detailed", "comprehensive":
		return "high"
	default:
		return "medium"
	}
}

func (h *Handler) getEffectiveEffort(req *model.ResponsesRequest) string {
	if req.Reasoning != nil {
		return req.Reasoning.Effort
	}
	return "medium"
}

// ===== Anthropic 格式转换 =====

func (h *Handler) anthropicToInternal(raw map[string]interface{}) model.ResponsesRequest {
	req := model.ResponsesRequest{
		Model:     getString(raw, "model", ""),
		Stream:    getBool(raw, "stream", false),
		MaxOutput: getInt(raw, "max_tokens", 4096),
	}

	// system prompt
	if sys, ok := raw["system"]; ok {
		if s, ok := sys.(string); ok {
			req.Instructions = s
		}
	}

	// messages 转 input
	if msgs, ok := raw["messages"].([]interface{}); ok {
		var items []interface{}
		for _, m := range msgs {
			if mMap, ok := m.(map[string]interface{}); ok {
				items = append(items, mMap)
			}
		}
		req.Input = items
	}

	// thinking
	if thinking, ok := raw["thinking"].(map[string]interface{}); ok {
		req.Thinking = &model.ThinkingConfig{
			Type:         getString(thinking, "type", "disabled"),
			BudgetTokens: getInt(thinking, "budget_tokens", 0),
		}
	}

	// tools
	if tools, ok := raw["tools"].([]interface{}); ok {
		for _, t := range tools {
			if tMap, ok := t.(map[string]interface{}); ok {
				tool := model.Tool{
					Type:        "function",
					Name:        getString(tMap, "name", ""),
					Description: getString(tMap, "description", ""),
				}
				if s, ok := tMap["input_schema"].(map[string]interface{}); ok {
					tool.Parameters = s
				}
				req.Tools = append(req.Tools, tool)
			}
		}
	}

	// temperature / top_p / top_k
	req.Temperature = getFloat(raw, "temperature", 0)
	req.TopP = getFloat(raw, "top_p", 0)
	if v, ok := raw["top_k"].(float64); ok {
		k := int(v)
		req.TopK = &k
	}

	// tool_choice
	if v, ok := raw["tool_choice"]; ok {
		req.ToolChoice = v
	}

	// stop_sequences
	if ss, ok := raw["stop_sequences"].([]interface{}); ok {
		for _, s := range ss {
			if str, ok := s.(string); ok {
				req.StopSequences = append(req.StopSequences, str)
			}
		}
	}

	// metadata (extract user_id)
	if m, ok := raw["metadata"].(map[string]interface{}); ok {
		meta := make(map[string]string, len(m))
		for k, val := range m {
			if s, ok := val.(string); ok {
				meta[k] = s
			}
		}
		req.Metadata = meta
	}

	// system as array of text blocks
	if sys, ok := raw["system"]; ok && req.Instructions == "" {
		if blocks, ok := sys.([]interface{}); ok {
			var parts []string
			for _, b := range blocks {
				if bMap, ok := b.(map[string]interface{}); ok {
					if t, ok := bMap["text"].(string); ok && t != "" {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				req.Instructions = strings.Join(parts, "\n\n")
			}
		}
	}

	// mcp_servers (not yet supported)
	if _, ok := raw["mcp_servers"]; ok {
		log.Printf("[WARN] mcp_servers present in /v1/messages request but not yet supported; ignoring")
	}

	return req
}

func (h *Handler) toAnthropicResponse(resp *model.ResponsesResponse) gin.H {
	var contentBlocks []gin.H
	var textParts []string
	hasToolCalls := false

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, block := range item.Content {
				if block.Text != "" || block.Content != "" {
					text := block.Text
					if text == "" {
						text = block.Content
					}
					textParts = append(textParts, text)
					contentBlocks = append(contentBlocks, gin.H{
						"type": "text",
						"text": text,
					})
				}
			}
		case "function_call":
			hasToolCalls = true
			argsMap := map[string]interface{}{}
			if s, ok := item.Arguments.(string); ok && s != "" {
				json.Unmarshal([]byte(s), &argsMap)
			}
			contentBlocks = append(contentBlocks, gin.H{
				"type":  "tool_use",
				"id":    item.CallID,
				"name":  item.Name,
				"input": argsMap,
			})
		}
	}

	stopReason := "end_turn"
	if hasToolCalls {
		stopReason = "tool_use"
	}

	allText := strings.Join(textParts, " ")
	return gin.H{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"content":       contentBlocks,
		"model":         resp.Model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": gin.H{
			"input_tokens":  0,
			"output_tokens": len(allText) + len(contentBlocks)*4,
		},
	}
}

// enrichResponse 用请求参数填充响应中的标准字段（echo fields）
// 符合 OpenAI Responses API 规范
func (h *Handler) enrichResponse(resp *model.ResponsesResponse, req *model.ResponsesRequest) {
	if resp.ID == "" {
		resp.ID = req.ResponseID
	}
	if resp.ID == "" {
		resp.ID = newResponseID()
	}
	if resp.Object == "" {
		resp.Object = "response"
	}
	if resp.Status == "" {
		resp.Status = "completed"
	}
	// 时间戳
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}

	// 回显请求参数
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if resp.Instructions == nil && req.Instructions != "" {
		resp.Instructions = &req.Instructions
	}
	if req.MaxOutput > 0 && resp.MaxOutputTokens == nil {
		maxOut := req.MaxOutput
		resp.MaxOutputTokens = &maxOut
	}
	if req.Temperature > 0 && resp.Temperature == nil {
		t := req.Temperature
		resp.Temperature = &t
	}
	if req.TopP > 0 && resp.TopP == nil {
		t := req.TopP
		resp.TopP = &t
	}
	if req.PreviousRespID != "" && resp.PreviousResponseID == nil {
		resp.PreviousResponseID = &req.PreviousRespID
	}
	if resp.Truncation == "" && req.Truncation != "" {
		resp.Truncation = req.Truncation
	}
	if resp.ParallelToolCalls == false {
		resp.ParallelToolCalls = req.ParallelToolCalls
	}
	if resp.Store == nil {
		s := req.Store
		resp.Store = &s
	}
	if req.User != "" && resp.User == nil {
		resp.User = &req.User
	}
	// metadata
	if req.Metadata != nil && resp.Metadata == nil {
		resp.Metadata = req.Metadata
	}
	// text.format
	if req.Text != nil && req.Text.Format != nil && resp.Text == nil {
		resp.Text = req.Text
	}
	// reasoning
	if req.Reasoning != nil && resp.Reasoning == nil {
		resp.Reasoning = req.Reasoning
	}
	// tools
	if len(req.Tools) > 0 && len(resp.Tools) == 0 {
		resp.Tools = req.Tools
	}
	if req.ToolChoice != nil && resp.ToolChoice == nil {
		resp.ToolChoice = req.ToolChoice
	}
	if resp.OutputText == "" {
		resp.OutputText = collectOutputText(resp.Output)
	}
	ensureOutputItemIDs(resp)
}

func (h *Handler) resolveMode(req *model.ResponsesRequest) string {
	if req.Mode != "" {
		return req.Mode
	}
	if h.config.DefaultMode == "react" || h.config.DefaultMode == "plan_execute" {
		return h.config.DefaultMode
	}
	return "react"
}

func (h *Handler) setupSSEHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

func (h *Handler) sendSSE(c *gin.Context, eventType string, data interface{}) {
	if data == nil {
		fmt.Fprintf(c.Writer, "event: %s\ndata: {\"type\":%q}\n\n", eventType, eventType)
		return
	}
	if payload, ok := data.(map[string]interface{}); ok {
		if _, exists := payload["type"]; !exists {
			payload["type"] = eventType
		}
	}
	if payload, ok := data.(gin.H); ok {
		if _, exists := payload["type"]; !exists {
			payload["type"] = eventType
		}
	}
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

func (h *Handler) sendRawSSE(c *gin.Context, eventType string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

// ===== JSON 辅助 =====

func getString(m map[string]interface{}, key, fallback string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return fallback
}

func firstNonEmptyString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func getBool(m map[string]interface{}, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

func getTopK(m map[string]interface{}) *int {
	switch v := m["top_k"].(type) {
	case float64:
		k := int(v)
		return &k
	case int:
		return &v
	}
	return nil
}

func getStringSlice(m map[string]interface{}, key string) []string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, s := range arr {
		if str, ok := s.(string); ok {
			out = append(out, str)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func collectOutputText(items []model.OutputItem) string {
	var parts []string
	for _, item := range items {
		if item.Type != "message" {
			continue
		}
		for _, block := range item.Content {
			switch {
			case block.Text != "":
				parts = append(parts, block.Text)
			case block.Content != "":
				parts = append(parts, block.Content)
			}
		}
	}
	return strings.Join(parts, "")
}

func (h *Handler) loadResponseContext(responseID string) ([]interface{}, bool) {
	h.storeMu.RLock()
	defer h.storeMu.RUnlock()

	items, ok := h.responseStore[responseID]
	if !ok {
		return nil, false
	}
	return append([]interface{}(nil), items...), true
}

func (h *Handler) storeResponseContext(responseID string, req *model.ResponsesRequest, resp *model.ResponsesResponse) {
	if responseID == "" {
		return
	}
	items := inputToContextItems(req.Input)
	items = append(items, responseOutputToContextItems(resp.Output)...)

	h.storeMu.Lock()
	defer h.storeMu.Unlock()
	if h.responseStore == nil {
		h.responseStore = make(map[string][]interface{})
	}
	if h.responses == nil {
		h.responses = make(map[string]*model.ResponsesResponse)
	}
	h.responseStore[responseID] = items
	h.responses[responseID] = resp
}

func mergeResponseContext(previous []interface{}, input interface{}) interface{} {
	merged := append([]interface{}(nil), previous...)
	merged = append(merged, inputToContextItems(input)...)
	return merged
}

func inputToContextItems(input interface{}) []interface{} {
	switch v := input.(type) {
	case nil:
		return nil
	case string:
		return []interface{}{map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": v,
		}}
	case []interface{}:
		return append([]interface{}(nil), v...)
	default:
		return []interface{}{map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": fmt.Sprintf("%v", v),
		}}
	}
}

func responseOutputToContextItems(output []model.OutputItem) []interface{} {
	items := make([]interface{}, 0, len(output))
	for _, item := range output {
		var raw map[string]interface{}
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		items = append(items, raw)
	}
	return items
}

func chatMessagesToResponsesInput(messages []map[string]interface{}) []interface{} {
	items := make([]interface{}, 0, len(messages))
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		switch role {
		case "tool":
			items = append(items, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": firstNonEmptyString(msg, "tool_call_id", "call_id", "id"),
				"output":  msg["content"],
			})
		case "assistant":
			if content, ok := msg["content"].(string); ok && content != "" {
				items = append(items, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": content,
				})
			}
			if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
				for _, rawCall := range toolCalls {
					toolCall, ok := rawCall.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := toolCall["function"].(map[string]interface{})
					items = append(items, map[string]interface{}{
						"type":      "function_call",
						"call_id":   firstNonEmptyString(toolCall, "id", "call_id"),
						"name":      getString(fn, "name", ""),
						"arguments": fn["arguments"],
					})
				}
			}
		default:
			if role == "" {
				role = "user"
			}
			items = append(items, map[string]interface{}{
				"type":    "message",
				"role":    role,
				"content": msg["content"],
			})
		}
	}
	return items
}

func parseResponseTools(toolsArr []interface{}) []model.Tool {
	parsed := make([]model.Tool, 0, len(toolsArr))
	for _, t := range toolsArr {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		tool := model.Tool{}
		if tt, ok := tm["type"].(string); ok {
			tool.Type = tt
		}
		if n, ok := tm["name"].(string); ok {
			tool.Name = n
		}
		if d, ok := tm["description"].(string); ok {
			tool.Description = d
		}
		if p, ok := tm["parameters"].(map[string]interface{}); ok {
			tool.Parameters = p
		}
		if s, ok := tm["strict"].(bool); ok {
			tool.Strict = &s
		}
		if fn, ok := tm["function"].(map[string]interface{}); ok {
			if tool.Type == "" {
				tool.Type = "function"
			}
			if tool.Name == "" {
				tool.Name = getString(fn, "name", "")
			}
			if tool.Description == "" {
				tool.Description = getString(fn, "description", "")
			}
			if tool.Parameters == nil {
				if p, ok := fn["parameters"].(map[string]interface{}); ok {
					tool.Parameters = p
				}
			}
			if tool.Strict == nil {
				if s, ok := fn["strict"].(bool); ok {
					tool.Strict = &s
				}
			}
		}
		if tool.Name != "" || tool.Type != "" {
			parsed = append(parsed, tool)
		}
	}
	return parsed
}

func ensureOutputItemIDs(resp *model.ResponsesResponse) {
	for i := range resp.Output {
		item := &resp.Output[i]
		if item.Type == "" {
			item.Type = "message"
		}
		if item.ID == "" {
			item.ID = outputItemID(resp.ID, item.Type, i)
		}
		if item.Type == "function_call" && item.CallID == "" {
			item.CallID = item.ID
		}
		if item.Type == "message" && item.Role == "" {
			item.Role = "assistant"
		}
		if item.Status == "" {
			item.Status = "completed"
		}
		for j := range item.Content {
			if item.Content[j].Type == "" {
				item.Content[j].Type = "output_text"
			}
		}
	}
}

func outputItemID(responseID, itemType string, index int) string {
	prefix := "item"
	switch itemType {
	case "message":
		prefix = "msg"
	case "function_call":
		prefix = "fc"
	case "function_call_output":
		prefix = "fco"
	}
	return fmt.Sprintf("%s_%s_%d", prefix, responseID, index)
}

func contentBlockTextValue(block model.ContentBlock) string {
	if block.Text != "" {
		return block.Text
	}
	return block.Content
}

func contentPartPayload(text string) gin.H {
	return gin.H{
		"type":        "output_text",
		"text":        text,
		"annotations": []interface{}{},
	}
}

func outputArgumentsString(arguments interface{}) string {
	switch v := arguments.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(v) == "" {
			return "{}"
		}
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}

func chunkString(s string, size int) []string {
	if s == "" {
		return nil
	}
	if size <= 0 {
		size = 256
	}
	runes := []rune(s)
	chunks := make([]string, 0, (len(runes)+size-1)/size)
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

func newResponseID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

func newChatCompletionID() string {
	return fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano())
}

func newMessageID() string {
	return "msg_" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func getInt(m map[string]interface{}, key string, fallback int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return fallback
}
