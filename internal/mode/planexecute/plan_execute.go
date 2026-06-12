package planexecute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

const (
	maxPlanSteps = 20 // 最大计划步骤数
)

// PlanExecuteEngine 实现 Plan & Execute 模式
// 类似 Claude Code 的 plan 模式：先制定完整计划，然后逐步执行
type PlanExecuteEngine struct {
	proxy    *proxy.Proxy
	registry *tools.ToolRegistry
}

func NewEngine(p *proxy.Proxy, r *tools.ToolRegistry) *PlanExecuteEngine {
	return &PlanExecuteEngine{
		proxy:    p,
		registry: r,
	}
}

// Process 处理 Plan & Execute 流程
func (e *PlanExecuteEngine) Process(ctx context.Context, req *model.ResponsesRequest) (*model.ResponsesResponse, error) {
	// 第一阶段：生成计划
	plan, err := e.generatePlan(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("plan generation failed: %w", err)
	}

	var outputItems []model.OutputItem

	// 第二阶段：执行计划
	executionResults, err := e.executePlan(ctx, req, plan, &outputItems)
	if err != nil {
		return nil, fmt.Errorf("plan execution failed: %w", err)
	}

	// 第三阶段：生成最终回答
	finalResponse, err := e.generateFinalResponse(ctx, req, plan, executionResults)
	if err != nil {
		return nil, fmt.Errorf("final response generation failed: %w", err)
	}

	outputItems = append(outputItems, model.OutputItem{
		Type:   "message",
		Role:   "assistant",
		Status: "completed",
		Content: []model.ContentBlock{
			{Type: "output_text", Text: finalResponse},
		},
	})

	responseID := req.ResponseID
	if responseID == "" {
		responseID = fmt.Sprintf("resp_plan_%d", len(outputItems))
	}

	return &model.ResponsesResponse{
		ID:     responseID,
		Object: "response",
		Status: "completed",
		Output: outputItems,
		Model:  req.Model,
	}, nil
}

// generatePlan 第一阶段：让 AI 制定完整计划
func (e *PlanExecuteEngine) generatePlan(ctx context.Context, req *model.ResponsesRequest) (string, error) {
	userInput := extractUserInput(req)

	planPrompt := fmt.Sprintf(`You are a planning assistant. Your task is to create a detailed plan to accomplish the user's request.

The user wants to: %s

Available tools: %s

Create a step-by-step plan. Each step should specify:
1. What action to take
2. Which tool to use (if any)
3. What input to provide

Format your response as a JSON array of steps:
[
  {"step": 1, "action": "...", "tool": "tool_name or null", "input": "..."},
  {"step": 2, "action": "...", "tool": "tool_name or null", "input": "..."}
]

Only output the JSON array, nothing else.`, userInput, strings.Join(e.registry.ListTools(), ", "))

	messages := []map[string]interface{}{
		{"role": "user", "content": planPrompt},
	}

	upstreamReq := map[string]interface{}{
		"model":       req.Model,
		"messages":    messages,
		"temperature": 0.3, // 低温度以获得更一致的输出
	}

	resp, err := e.proxy.ForwardRequest(upstreamReq)
	if err != nil {
		return "", fmt.Errorf("upstream API error: %w", err)
	}

	choice := resp["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	content, _ := message["content"].(string)

	return content, nil
}

// executePlan 第二阶段：执行计划中的每个步骤
func (e *PlanExecuteEngine) executePlan(ctx context.Context, req *model.ResponsesRequest, planJSON string, outputItems *[]model.OutputItem) (string, error) {
	// 解析计划
	var steps []map[string]interface{}

	// 尝试从响应中提取 JSON
	jsonStr := extractJSON(planJSON)
	if err := json.Unmarshal([]byte(jsonStr), &steps); err != nil {
		return "", fmt.Errorf("failed to parse plan: %w", err)
	}

	var results []string

	for _, step := range steps {
		if len(results) >= maxPlanSteps {
			break
		}

		stepNum, _ := step["step"].(float64)
		action, _ := step["action"].(string)
		toolName, _ := step["tool"].(string)
		input, _ := step["input"].(string)

		var result string
		if toolName != "" && toolName != "null" {
			// 执行工具
			args := map[string]interface{}{"input": input}
			toolResult, err := e.registry.Execute(ctx, toolName, args)
			if err != nil {
				result = fmt.Sprintf("Step %d (%s): Error - %v", int(stepNum), action, err)
			} else {
				result = fmt.Sprintf("Step %d (%s): %s", int(stepNum), action, toolResult)

				*outputItems = append(*outputItems, model.OutputItem{
					Type:   "tool_call",
					Name:   toolName,
					Status: "completed",
				})
			}
		} else {
			result = fmt.Sprintf("Step %d (%s): [No tool needed]", int(stepNum), action)
		}

		results = append(results, result)
	}

	return strings.Join(results, "\n"), nil
}

// generateFinalResponse 第三阶段：基于计划执行结果生成最终回答
func (e *PlanExecuteEngine) generateFinalResponse(ctx context.Context, req *model.ResponsesRequest, plan string, executionResults string) (string, error) {
	userInput := extractUserInput(req)

	finalPrompt := fmt.Sprintf(`You executed the following plan:

Plan:
%s

Execution Results:
%s

Based on the execution results above, provide a comprehensive answer to the user's original request:

%s

Synthesize all the information and provide a clear, complete answer.`, plan, executionResults, userInput)

	messages := []map[string]interface{}{
		{"role": "user", "content": finalPrompt},
	}

	upstreamReq := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
	}

	resp, err := e.proxy.ForwardRequest(upstreamReq)
	if err != nil {
		return "", fmt.Errorf("upstream API error: %w", err)
	}

	choice := resp["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	content, _ := message["content"].(string)

	return content, nil
}

// extractJSON 从文本中提取 JSON
func extractJSON(text string) string {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

func extractUserInput(req *model.ResponsesRequest) string {
	switch input := req.Input.(type) {
	case string:
		return input
	case []interface{}:
		var parts []string
		for _, item := range input {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if content, ok := itemMap["content"].(string); ok {
					parts = append(parts, content)
					continue
				}
				if blocks, ok := itemMap["content"].([]interface{}); ok {
					for _, rawBlock := range blocks {
						block, ok := rawBlock.(map[string]interface{})
						if !ok {
							continue
						}
						if text, ok := block["text"].(string); ok {
							parts = append(parts, text)
						}
					}
				}
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}
