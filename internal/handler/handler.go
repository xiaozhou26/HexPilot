package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	}
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// OpenAI Responses API 兼容接口
	r.POST("/v1/responses", h.HandleResponses)

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
	if v, ok := rawBody["input"]; ok {
		req.Input = v
	}
	if v, ok := rawBody["stream"].(bool); ok {
		req.Stream = v
	}
	if v, ok := rawBody["max_output_tokens"].(float64); ok {
		req.MaxOutput = int(v)
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
	// reasoning
	if r, ok := rawBody["reasoning"].(map[string]interface{}); ok {
		req.Reasoning = &model.ReasoningConfig{}
		if e, ok := r["effort"].(string); ok {
			req.Reasoning.Effort = e
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
	// text
	if tx, ok := rawBody["text"].(map[string]interface{}); ok {
		req.Text = &model.TextConfig{}
		if v, ok := tx["verbosity"].(string); ok {
			req.Text.Verbosity = v
		}
	}
	// tools
	if toolsArr, ok := rawBody["tools"].([]interface{}); ok {
		for _, t := range toolsArr {
			if tm, ok := t.(map[string]interface{}); ok {
				tool := model.Tool{}
				if tt, ok := tm["type"].(string); ok {
					tool.Type = tt
				}
				if fn, ok := tm["function"].(map[string]interface{}); ok {
					if n, ok := fn["name"].(string); ok {
						tool.Name = n
					}
					if d, ok := fn["description"].(string); ok {
						tool.Description = d
					}
				}
				req.Tools = append(req.Tools, tool)
			}
		}
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

	req.RawBody = rawBody

	// 规范化思考配置（统一映射）
	h.normalizeReasoning(req)

	log.Printf("[/v1/responses] model=%s, mode=%s, effort=%s, stream=%v",
		req.Model, h.resolveMode(req), h.getEffectiveEffort(req), req.Stream)

	mode := h.resolveMode(req)

	if req.Stream {
		h.handleResponsesStream(c, req, mode)
	} else {
		h.handleResponsesSync(c, req, mode)
	}
}

// HandleMessages 处理 Anthropic /v1/messages 请求
// 将 Anthropic 格式转换为内部格式，然后通过 ReAct/Plan 引擎处理
func (h *Handler) HandleMessages(c *gin.Context) {
	var rawReq map[string]interface{}
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

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

	// 检查是否有 tools 定义
	if tools, ok := req["tools"].([]interface{}); ok && len(tools) > 0 {
		// 有工具，使用 ReAct 循环处理
		result, err := h.chatEngine.Process(c.Request.Context(), req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}

	// 没有工具，直接转发到上游
	result, err := h.proxy.ForwardRequest(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// HealthCheck 健康检查
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
		"mode":   h.config.DefaultMode,
		"endpoints": []string{
			"POST /v1/responses",
			"POST /v1/messages",
			"POST /v1/chat/completions",
			"GET  /v1/models",
			"GET  /health",
		},
	})
}

// ListModels 列出模型（欺骗客户端）
func (h *Handler) ListModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data": []gin.H{
			{
				"id":       "gpt-5",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "openai",
			},
			{
				"id":       "claude-opus-4-8",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "anthropic",
			},
		},
	})
}

// ===== 内部处理 =====

func (h *Handler) handleResponsesSync(c *gin.Context, req *model.ResponsesRequest, mode string) {
	result, err := h.executeMode(c, req, mode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) handleResponsesStream(c *gin.Context, req *model.ResponsesRequest, mode string) {
	h.setupSSEHeaders(c)
	h.sendSSE(c, "response.created", nil)

	err := h.streamMode(c, req, mode, func(event *model.SSEEvent) error {
		h.sendSSE(c, event.Type, event.Data)
		c.Writer.Flush()
		return nil
	})

	if err != nil {
		h.sendSSE(c, "error", gin.H{"error": err.Error()})
		return
	}
	h.sendSSE(c, "response.completed", nil)
	c.Writer.Flush()
}

func (h *Handler) handleAnthropicSync(c *gin.Context, req *model.ResponsesRequest, mode string) {
	result, err := h.executeMode(c, req, mode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 转换为 Anthropic 格式响应
	c.JSON(http.StatusOK, h.toAnthropicResponse(result))
}

func (h *Handler) handleAnthropicStream(c *gin.Context, req *model.ResponsesRequest, mode string) {
	h.setupSSEHeaders(c)

	// Anthropic 流式事件
	h.sendRawSSE(c, "message_start", gin.H{
		"type": "message",
		"role": "assistant",
	})

	err := h.streamMode(c, req, mode, func(event *model.SSEEvent) error {
		h.sendRawSSE(c, "content_block_delta", gin.H{
			"index": 0,
			"delta": gin.H{
				"type": "text_delta",
				"text": fmt.Sprintf("%v", event.Data),
			},
		})
		c.Writer.Flush()
		return nil
	})

	if err != nil {
		h.sendRawSSE(c, "error", gin.H{"message": err.Error()})
		return
	}
	h.sendRawSSE(c, "message_delta", gin.H{
		"stop_reason": "end_turn",
	})
	h.sendRawSSE(c, "message_stop", nil)
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

func (h *Handler) streamMode(c *gin.Context, req *model.ResponsesRequest, mode string, onEvent func(*model.SSEEvent) error) error {
	switch mode {
	case "plan_execute":
		return h.streamPlanExecute(c, req, onEvent)
	case "react":
		fallthrough
	default:
		return h.reactEngine.ProcessStream(c.Request.Context(), req, onEvent)
	}
}

func (h *Handler) streamPlanExecute(c *gin.Context, req *model.ResponsesRequest, onEvent func(*model.SSEEvent) error) error {
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
				req.Tools = append(req.Tools, tool)
			}
		}
	}

	return req
}

func (h *Handler) toAnthropicResponse(resp *model.ResponsesResponse) gin.H {
	var contentBlocks []gin.H
	var textParts []string
	for _, item := range resp.Output {
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
	}

	return gin.H{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"content":       contentBlocks,
		"model":         resp.Model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": gin.H{
			"input_tokens":  0,
			"output_tokens": len(strings.Join(textParts, " ")),
		},
	}
}

// ===== 工具函数 =====

func (h *Handler) resolveMode(req *model.ResponsesRequest) string {
	if req.Mode != "" {
		return req.Mode
	}
	return h.config.DefaultMode
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
		fmt.Fprintf(c.Writer, "event: %s\ndata: {}\n\n", eventType)
		return
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

func getBool(m map[string]interface{}, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
