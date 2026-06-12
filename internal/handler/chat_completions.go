package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"

	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

const (
	chatMaxToolCalls = 10
)

// ChatCompletionsEngine 处理 OpenAI chat/completions 格式的工具调用循环
type ChatCompletionsEngine struct {
	proxy    *proxy.Proxy
	registry *tools.ToolRegistry
}

func NewChatEngine(p *proxy.Proxy, r *tools.ToolRegistry) *ChatCompletionsEngine {
	return &ChatCompletionsEngine{
		proxy:    p,
		registry: r,
	}
}

// Process 处理聊天完成请求，支持工具调用循环
func (e *ChatCompletionsEngine) Process(ctx context.Context, rawReq map[string]interface{}) (map[string]interface{}, error) {
	// 提取请求参数
	messages := extractMessages(rawReq)
	toolDefs := extractToolDefs(rawReq)
	model := getString(rawReq, "model", "")
	temperature := getFloat(rawReq, "temperature", 1.0)
	maxTokens := getInt(rawReq, "max_tokens", 4096)

	log.Printf("[CHAT ENGINE] model=%s, messages_count=%d, tools_count=%d, temp=%.1f, max_tokens=%d",
		model, len(messages), len(toolDefs), temperature, maxTokens)

	// 构建上游请求基础
	baseUpstreamReq := map[string]interface{}{
		"model":       model,
		"temperature": temperature,
		"max_tokens":  maxTokens,
	}

	// 工具调用循环
	for i := 0; i < chatMaxToolCalls; i++ {
		upstreamReq := make(map[string]interface{})
		for k, v := range baseUpstreamReq {
			upstreamReq[k] = v
		}
		upstreamReq["messages"] = messages

		// 只在第一轮传递工具定义
		if len(toolDefs) > 0 && i == 0 {
			upstreamReq["tools"] = toolDefs
			if tc, ok := rawReq["tool_choice"]; ok {
				upstreamReq["tool_choice"] = tc
			}
		}

		// 转发到上游 API
		resp, err := e.proxy.ForwardRequest(upstreamReq)
		if err != nil {
			return nil, fmt.Errorf("upstream API error: %w", err)
		}

		// 解析响应
		choices, ok := resp["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return nil, fmt.Errorf("invalid upstream response: no choices")
		}

		firstChoice := choices[0].(map[string]interface{})
		message := firstChoice["message"].(map[string]interface{})

		// 检查是否有 tool_calls
		toolCalls, hasToolCalls := message["tool_calls"].([]interface{})
		if !hasToolCalls || len(toolCalls) == 0 {
			// 没有工具调用，直接返回上游响应
			return resp, nil
		}

		// 有工具调用，执行工具
		var toolResults []map[string]interface{}
		for _, tc := range toolCalls {
			toolCall := tc.(map[string]interface{})
			callID, _ := toolCall["id"].(string)
			function := toolCall["function"].(map[string]interface{})
			toolName, _ := function["name"].(string)
			argsStr, _ := function["arguments"].(string)

			var args map[string]interface{}
			json.Unmarshal([]byte(argsStr), &args)

			result, err := e.registry.Execute(ctx, toolName, args)
			if err != nil {
				result = fmt.Sprintf("Error executing tool %s: %v", toolName, err)
			}

			toolResults = append(toolResults, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      result,
			})
		}

		// 将 assistant 消息（包含 tool_calls）和 tool 结果追加到消息历史
		messages = append(messages, message)
		for _, tr := range toolResults {
			messages = append(messages, tr)
		}
	}

	// 达到最大工具调用次数，返回当前状态
	return map[string]interface{}{
		"id":     fmt.Sprintf("chatcmpl-%d", rand.Intn(80809)),
		"object": "chat.completion",
		"model":  model,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Maximum tool calls reached.",
				},
				"finish_reason": "length",
			},
		},
	}, nil
}

func extractMessages(req map[string]interface{}) []map[string]interface{} {
	msgs, ok := req["messages"].([]interface{})
	if !ok {
		return nil
	}

	var result []map[string]interface{}
	for _, m := range msgs {
		if msgMap, ok := m.(map[string]interface{}); ok {
			result = append(result, msgMap)
		}
	}
	return result
}

func extractToolDefs(req map[string]interface{}) []map[string]interface{} {
	toolsRaw, ok := req["tools"].([]interface{})
	if !ok {
		return nil
	}

	var result []map[string]interface{}
	for _, t := range toolsRaw {
		if tMap, ok := t.(map[string]interface{}); ok {
			result = append(result, tMap)
		}
	}
	return result
}

func getFloat(m map[string]interface{}, key string, fallback float64) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return fallback
}

// ToChatCompletionResponse 将 ReAct 引擎的 ResponsesResponse 转换为 chat/completions 格式
func ToChatCompletionResponse(resp *model.ResponsesResponse) map[string]interface{} {
	var contentParts []string
	var toolCalls []interface{}

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, block := range item.Content {
				if block.Text != "" {
					contentParts = append(contentParts, block.Text)
				} else if block.Content != "" {
					contentParts = append(contentParts, block.Content)
				}
			}
		case "tool_call", "function_call":
			args := "{}"
			if item.Arguments != nil {
				switch value := item.Arguments.(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						args = value
					}
				default:
					data, err := json.Marshal(value)
					if err == nil {
						args = string(data)
					}
				}
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      item.Name,
					"arguments": args,
				},
			})
		}
	}

	message := map[string]interface{}{
		"role": "assistant",
	}

	if len(contentParts) > 0 {
		message["content"] = strings.Join(contentParts, "\n")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return map[string]interface{}{
		"id":     resp.ID,
		"object": "chat.completion",
		"model":  resp.Model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
}
