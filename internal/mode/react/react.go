package react

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

const (
	maxToolCalls = 10 // 最大工具调用次数，防止无限循环
)

// ReActEngine 实现 ReAct (Reason + Act) 模式
type ReActEngine struct {
	proxy    *proxy.Proxy
	registry *tools.ToolRegistry
}

func NewEngine(p *proxy.Proxy, r *tools.ToolRegistry) *ReActEngine {
	return &ReActEngine{
		proxy:    p,
		registry: r,
	}
}

// Process 处理 ReAct 循环
func (e *ReActEngine) Process(ctx context.Context, req *model.ResponsesRequest) (*model.ResponsesResponse, error) {
	// 第一步：将请求转换为 OpenAI chat 格式，包含工具定义
	messages := buildInitialMessages(req)
	toolDefs := convertToolsToOpenAIFormat(e.registry)

	var outputItems []model.OutputItem

	// ReAct 循环
	for i := 0; i < maxToolCalls; i++ {
		// 构建当前轮次的请求
		upstreamReq := map[string]interface{}{
			"model":    req.Model,
			"messages": messages,
		}
		if len(toolDefs) > 0 && i == 0 {
			// 只在第一轮传递工具定义（让 AI 知道有哪些工具可用）
			upstreamReq["tools"] = toolDefs
			upstreamReq["tool_choice"] = req.ToolChoice
		}

		// 转发到上游 API
		resp, err := e.proxy.ForwardRequest(upstreamReq)
		if err != nil {
			return nil, fmt.Errorf("upstream API error: %w", err)
		}

		// 解析响应
		choice, ok := resp["choices"].([]interface{})
		if !ok || len(choice) == 0 {
			return nil, fmt.Errorf("invalid upstream response: no choices")
		}

		firstChoice := choice[0].(map[string]interface{})
		message := firstChoice["message"].(map[string]interface{})

		// 检查是否包含 tool_calls
		toolCalls, hasToolCalls := message["tool_calls"].([]interface{})

		if !hasToolCalls || len(toolCalls) == 0 {
			// 没有工具调用，返回最终文本响应
			content, _ := message["content"].(string)
			outputItems = append(outputItems, model.OutputItem{
				Type: "message",
				Role: "assistant",
				Content: []model.ContentBlock{
					{Type: "text", Text: content},
				},
			})
			break
		}

		// 有工具调用，执行工具
		var callResults []map[string]interface{}
		for _, tc := range toolCalls {
			toolCall := tc.(map[string]interface{})
			callID, _ := toolCall["id"].(string)
			function := toolCall["function"].(map[string]interface{})
			toolName, _ := function["name"].(string)
			argsStr, _ := function["arguments"].(string)

			// 解析参数
			var args map[string]interface{}
			json.Unmarshal([]byte(argsStr), &args)

			// 执行工具
			result, err := e.registry.Execute(ctx, toolName, args)
			if err != nil {
				result = fmt.Sprintf("Error: %v", err)
			}

			// 记录工具调用
			outputItems = append(outputItems, model.OutputItem{
				Type:   "tool_call",
				ID:     callID,
				Name:   toolName,
				CallID: callID,
				Status: "completed",
			})

			callResults = append(callResults, map[string]interface{}{
				"tool_call_id": callID,
				"role":         "tool",
				"content":      result,
			})
		}

		// 将工具调用和结果添加到消息历史
		messages = append(messages, message)
		for _, cr := range callResults {
			messages = append(messages, cr)
		}
	}

	return &model.ResponsesResponse{
		ID:     fmt.Sprintf("resp_react_%d", len(outputItems)),
		Object: "response",
		Status: "completed",
		Output: outputItems,
		Model:  req.Model,
	}, nil
}

// ProcessStream 处理 ReAct 循环（流式）
func (e *ReActEngine) ProcessStream(ctx context.Context, req *model.ResponsesRequest, onEvent func(*model.SSEEvent) error) error {
	messages := buildInitialMessages(req)
	toolDefs := convertToolsToOpenAIFormat(e.registry)

	// 简化版流式实现：每轮单独流式处理
	for i := 0; i < maxToolCalls; i++ {
		upstreamReq := map[string]interface{}{
			"model":    req.Model,
			"messages": messages,
		}
		if len(toolDefs) > 0 && i == 0 {
			upstreamReq["tools"] = toolDefs
		}

		var contentBuilder strings.Builder
		var toolCalls []map[string]interface{}
		var currentToolCall map[string]interface{}

		err := e.proxy.ForwardRequestStream(upstreamReq, func(chunk []byte) error {
			lines := strings.Split(string(chunk), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "data: ") {
					data := line[6:]
					if data == "[DONE]" {
						continue
					}

					var event map[string]interface{}
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						return nil
					}

					choices, ok := event["choices"].([]interface{})
					if !ok || len(choices) == 0 {
						continue
					}

					choice := choices[0].(map[string]interface{})
					delta := choice["delta"].(map[string]interface{})

					// 处理工具调用
					if tc, exists := delta["tool_calls"]; exists {
						tcList := tc.([]interface{})
						for _, t := range tcList {
							tMap := t.(map[string]interface{})
							if id, ok := tMap["id"].(string); ok && id != "" {
								if currentToolCall != nil {
									toolCalls = append(toolCalls, currentToolCall)
								}
								currentToolCall = tMap
							} else if currentToolCall != nil {
								// 合并参数
								if fn, ok := tMap["function"].(map[string]interface{}); ok {
									if existingFn, ok := currentToolCall["function"].(map[string]interface{}); ok {
										if args, ok := fn["arguments"].(string); ok {
											existingArgs, _ := existingFn["arguments"].(string)
											existingFn["arguments"] = existingArgs + args
										}
									}
								}
							}
						}
					}

					// 处理文本内容
					if content, ok := delta["content"].(string); ok && content != "" {
						contentBuilder.WriteString(content)
						onEvent(&model.SSEEvent{
							Type: "response.output_text.delta",
							Data: map[string]interface{}{
								"delta": content,
							},
						})
					}
				}
			}
			return nil
		})

		if err != nil {
			return fmt.Errorf("stream error: %w", err)
		}

		// 如果有工具调用，执行它们
		if len(toolCalls) > 0 {
			// 添加最后的工具调用
			if currentToolCall != nil {
				toolCalls = append(toolCalls, currentToolCall)
			}

			var callResults []map[string]interface{}
			for _, tc := range toolCalls {
				function := tc["function"].(map[string]interface{})
				toolName, _ := function["name"].(string)
				argsStr, _ := function["arguments"].(string)

				var args map[string]interface{}
				json.Unmarshal([]byte(argsStr), &args)

				callID, _ := tc["id"].(string)
				result, err := e.registry.Execute(ctx, toolName, args)
				if err != nil {
					result = fmt.Sprintf("Error: %v", err)
				}

				callResults = append(callResults, map[string]interface{}{
					"tool_call_id": callID,
					"role":         "tool",
					"content":      result,
				})
			}

			messages = append(messages, map[string]interface{}{
				"role":       "assistant",
				"tool_calls": toolCalls,
			})
			for _, cr := range callResults {
				messages = append(messages, cr)
			}

			continue // 继续下一轮
		}

		// 没有工具调用，结束
		break
	}

	return nil
}

func buildInitialMessages(req *model.ResponsesRequest) []map[string]interface{} {
	var messages []map[string]interface{}

	log.Printf("[buildInitialMessages] Input type=%T, value=%v", req.Input, req.Input)

	// 添加系统指令
	if req.Instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": req.Instructions,
		})
	}

	// 处理输入
	switch input := req.Input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": input,
		})
	case []interface{}:
		for _, item := range input {
			if itemMap, ok := item.(map[string]interface{}); ok {
				messages = append(messages, itemMap)
			}
		}
	default:
		log.Printf("[buildInitialMessages] Unknown input type: %T", req.Input)
	}

	return messages
}

func convertToolsToOpenAIFormat(registry *tools.ToolRegistry) []map[string]interface{} {
	toolNames := registry.ListTools()
	var tools []map[string]interface{}

	for _, name := range toolNames {
		tool, _ := registry.GetTool(name)
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	return tools
}
