package react

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

const maxToolCalls = 10
const maxToolDescriptionRunes = 180

const thinkingModePrompt = `You are a reasoning agent that solves problems step by step.

IMPORTANT: Before answering, think deeply about the problem inside <thought> tags.
Then take actions using the available tools. Continue until you reach a final answer.
Do not return an empty message.

<thought>Your step-by-step reasoning here</thought>
<action>{"name": "tool_name", "arguments": {...}}</action>
<observation>Result from the tool</observation>
<final_answer>Your final comprehensive answer</final_answer>

Rules:
- Always think before acting
- Use tools when you need information
- When you have enough information, provide the final answer

Available tools:
%s`

const simpleModePrompt = `You are a helpful assistant with access to tools.
When you need a tool, output exactly one action and no final answer:
<action>{"name": "tool_name", "arguments": {...}}</action>

Do not use Markdown fences around the action. Do not return an empty message.
For file or command tasks on Windows, prefer shell_command with a PowerShell command when that tool is available.

When you have enough information, answer directly. You may wrap the final text in <final_answer>...</final_answer>.

Available tools:
%s`

const emptyResponseRetryPrompt = `Your previous response was empty.
Reply now with exactly one <action>{"name":"tool_name","arguments":{...}}</action> if a tool is needed, or a short final answer if no tool is needed.
Use only these tool names: %s.
For file creation, editing, inspection, or shell work on Windows, call shell_command with a PowerShell command when it is available.
Do not return an empty message.`

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

func (e *ReActEngine) Process(ctx context.Context, req *model.ResponsesRequest) (*model.ResponsesResponse, error) {
	toolList := formatToolListFromRequest(req)
	toolDefs := convertRequestToolsToOpenAI(req)
	messages := buildInitialMessages(req, toolList)

	var outputItems []model.OutputItem
	emptyRetryUsed := false
	for i := 0; i < maxToolCalls; i++ {
		upstreamReq := buildUpstreamRequest(req, messages)
		if req.UpstreamNativeTools && len(toolDefs) > 0 && i == 0 {
			upstreamReq["tools"] = toolDefs
			if req.ToolChoice != nil {
				upstreamReq["tool_choice"] = convertToolChoiceToOpenAIChat(req.ToolChoice)
			}
		}

		resp, err := e.proxy.ForwardRequest(upstreamReq)
		if err != nil {
			return nil, fmt.Errorf("upstream API error: %w", err)
		}

		message, err := firstChoiceMessage(resp)
		if err != nil {
			return nil, err
		}

		if items := messageToolCallsToResponseItems(message, req.Tools, len(outputItems)); len(items) > 0 {
			outputItems = append(outputItems, items...)
			break
		}

		content, _ := message["content"].(string)
		if action, ok := parseAction(content); ok && requestHasTool(req.Tools, action.Name) {
			outputItems = append(outputItems, actionToResponseFunctionCall(action, len(outputItems)))
			break
		}

		content = extractFinalAnswer(content)
		if content == "" {
			if len(req.Tools) > 0 && !emptyRetryUsed {
				emptyRetryUsed = true
				messages = append(messages, map[string]interface{}{
					"role":    "user",
					"content": fmt.Sprintf(emptyResponseRetryPrompt, strings.Join(requestToolNames(req.Tools), ", ")),
				})
				continue
			}
			content = "No response content returned by upstream."
		}
		outputItems = append(outputItems, model.OutputItem{
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []model.ContentBlock{
				{Type: "output_text", Text: content},
			},
		})
		break
	}

	if len(outputItems) == 0 {
		outputItems = append(outputItems, model.OutputItem{
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []model.ContentBlock{
				{Type: "output_text", Text: "Maximum tool calls reached."},
			},
		})
	}

	return &model.ResponsesResponse{
		ID:                responseID(req, "resp_react"),
		Object:            "response",
		Status:            "completed",
		Output:            outputItems,
		Model:             req.Model,
		ParallelToolCalls: req.ParallelToolCalls,
		Usage:             buildUsage(req, outputItems),
	}, nil
}

func (e *ReActEngine) executeNativeToolCalls(ctx context.Context, req *model.ResponsesRequest, toolCalls []interface{}, outputItems *[]model.OutputItem) []map[string]interface{} {
	var callResults []map[string]interface{}
	for _, rawCall := range toolCalls {
		toolCall, ok := rawCall.(map[string]interface{})
		if !ok {
			continue
		}
		callID, toolName, argsStr := parseChatToolCall(toolCall)
		args := parseArguments(argsStr)
		result := executeTool(ctx, e.registry, req.Tools, toolName, args)

		*outputItems = append(*outputItems, model.OutputItem{
			Type:      "function_call",
			ID:        callID,
			Name:      toolName,
			CallID:    callID,
			Arguments: argsStr,
			Status:    "completed",
		})

		callResults = append(callResults, map[string]interface{}{
			"tool_call_id": callID,
			"role":         "tool",
			"content":      result,
		})
	}
	return callResults
}

func chatToolCallsToResponseItems(toolCalls []interface{}, offset int) []model.OutputItem {
	items := make([]model.OutputItem, 0, len(toolCalls))
	for i, rawCall := range toolCalls {
		toolCall, ok := rawCall.(map[string]interface{})
		if !ok {
			continue
		}
		callID, toolName, argsStr := parseChatToolCall(toolCall)
		if callID == "" {
			callID = fmt.Sprintf("call_%d", offset+i+1)
		}
		items = append(items, model.OutputItem{
			Type:      "function_call",
			ID:        fmt.Sprintf("fc_%s", callID),
			Name:      toolName,
			CallID:    callID,
			Arguments: argsStr,
			Status:    "completed",
		})
	}
	return items
}

func messageToolCallsToResponseItems(message map[string]interface{}, reqTools []model.Tool, offset int) []model.OutputItem {
	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok || len(toolCalls) == 0 {
		return nil
	}

	items := chatToolCallsToResponseItems(toolCalls, offset)
	filtered := items[:0]
	for _, item := range items {
		if requestHasTool(reqTools, item.Name) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func actionToResponseFunctionCall(action actionCall, offset int) model.OutputItem {
	callID := fmt.Sprintf("call_%d", offset+1)
	return model.OutputItem{
		Type:      "function_call",
		ID:        fmt.Sprintf("fc_%s", callID),
		Name:      action.Name,
		CallID:    callID,
		Arguments: stringifyJSONValue(action.Arguments),
		Status:    "completed",
	}
}

func requestHasTool(reqTools []model.Tool, name string) bool {
	for _, t := range reqTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

type actionCall struct {
	Name      string
	Arguments map[string]interface{}
}

func parseAction(content string) (actionCall, bool) {
	payload, ok := extractTaggedContent(content, "action")
	if !ok {
		return actionCall{}, false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return actionCall{}, false
	}
	name, _ := raw["name"].(string)
	if name == "" {
		return actionCall{}, false
	}
	args, _ := raw["arguments"].(map[string]interface{})
	if args == nil {
		args = map[string]interface{}{}
	}
	return actionCall{Name: name, Arguments: args}, true
}

func extractFinalAnswer(content string) string {
	if answer, ok := extractTaggedContent(content, "final_answer"); ok {
		return strings.TrimSpace(answer)
	}
	return strings.TrimSpace(content)
}

func extractTaggedContent(content, tag string) (string, bool) {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	start := strings.Index(content, openTag)
	if start < 0 {
		return "", false
	}
	start += len(openTag)
	end := strings.Index(content[start:], closeTag)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(content[start : start+end]), true
}

func executeTool(ctx context.Context, registry *tools.ToolRegistry, reqTools []model.Tool, name string, args map[string]interface{}) string {
	if t, ok := registry.GetTool(name); ok {
		result, err := t.Handler(ctx, args)
		if err != nil {
			return fmt.Sprintf("Error executing %s: %v", name, err)
		}
		return result
	}

	for _, t := range reqTools {
		if t.Name == name {
			return fmt.Sprintf("[Request tool] %s called with args: %v", name, args)
		}
	}

	return fmt.Sprintf("Error: unknown tool '%s'", name)
}

func (e *ReActEngine) ProcessStream(ctx context.Context, req *model.ResponsesRequest, onEvent func(*model.SSEEvent) error, onToolCall func(map[string]interface{})) error {
	toolList := formatToolListFromRequest(req)
	toolDefs := convertRequestToolsToOpenAI(req)
	messages := buildInitialMessages(req, toolList)

	for i := 0; i < maxToolCalls; i++ {
		upstreamReq := buildUpstreamRequest(req, messages)
		if req.UpstreamNativeTools && len(toolDefs) > 0 && i == 0 {
			upstreamReq["tools"] = toolDefs
			if req.ToolChoice != nil {
				upstreamReq["tool_choice"] = convertToolChoiceToOpenAIChat(req.ToolChoice)
			}
		}

		var contentBuilder strings.Builder
		var toolCalls []map[string]interface{}
		var currentToolCall map[string]interface{}

		err := e.proxy.ForwardRequestStream(upstreamReq, func(chunk []byte) error {
			chunkStr := string(chunk)
			if strings.HasPrefix(chunkStr, "data: ") {
				// SSE path: each chunk is a "data: {...}" line
				data := strings.TrimPrefix(chunkStr, "data: ")
				if data == "[DONE]" {
					return nil
				}
				delta, err := parseDeltaFromSSEData(data)
				if err != nil {
					return nil
				}
				if delta == nil {
					return nil
				}
				mergeStreamingToolCalls(delta, &currentToolCall, &toolCalls)
				if content, ok := delta["content"].(string); ok && content != "" {
					contentBuilder.WriteString(content)
					if evErr := onEvent(&model.SSEEvent{
						Type: "response.output_text.delta",
						Data: map[string]interface{}{"delta": content},
					}); evErr != nil {
						return evErr
					}
				}
			} else {
				// Non-SSE path: full JSON body in one chunk
				processUpstreamJSON(chunk, &currentToolCall, &toolCalls, &contentBuilder, onEvent)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("stream error: %w", err)
		}

		if currentToolCall != nil {
			toolCalls = append(toolCalls, currentToolCall)
		}
		if len(toolCalls) == 0 {
			// In the non-native-tools path, the model emits <action>...</action>
			// tags in the text. Check for that before bailing.
			content := contentBuilder.String()
			if action, ok := parseAction(content); ok && requestHasTool(req.Tools, action.Name) {
				toolCalls = []map[string]interface{}{{
					"id":   "call_" + strconv.FormatInt(time.Now().UnixNano(), 10),
					"type": "function",
					"function": map[string]interface{}{
						"name":      action.Name,
						"arguments": argumentsJSONString(action.Arguments),
					},
				}}
			}
		}

		if len(toolCalls) == 0 {
			break
		}

		// When onToolCall is provided (Anthropic streaming path), report tool
		// calls to the handler and stop -- the client (Claude Code) will resume
		// with a follow-up request that includes tool_result blocks. The server
		// does NOT execute tools on the client's behalf in this path.
		if onToolCall != nil {
			for _, tc := range toolCalls {
				onToolCall(tc)
			}
			break
		}

		var callResults []map[string]interface{}
		for _, tc := range toolCalls {
			callID, toolName, argsStr := parseChatToolCall(tc)
			result := executeTool(ctx, e.registry, req.Tools, toolName, parseArguments(argsStr))
			callResults = append(callResults, map[string]interface{}{
				"tool_call_id": callID,
				"role":         "tool",
				"content":      result,
			})
		}

		messages = append(messages, map[string]interface{}{
			"role":       "assistant",
			"tool_calls": mapsToInterfaces(toolCalls),
		})
		for _, cr := range callResults {
			messages = append(messages, cr)
		}
	}

	return nil
}

func buildSystemPrompt(req *model.ResponsesRequest, toolList string) string {
	var prompt string
	if req.IsThinkingEnabled() {
		prompt = fmt.Sprintf(thinkingModePrompt, toolList)
	} else {
		prompt = fmt.Sprintf(simpleModePrompt, toolList)
	}
	if instruction := toolChoiceInstruction(req.ToolChoice); instruction != "" {
		prompt += "\n\n" + instruction
	}
	return prompt
}

func buildInitialMessages(req *model.ResponsesRequest, toolList string) []map[string]interface{} {
	var messages []map[string]interface{}

	systemPrompt := buildSystemPrompt(req, toolList)
	var instructions []string
	if req.Instructions != "" {
		instructions = append(instructions, req.Instructions)
	}
	if req.Prompt != "" {
		instructions = append(instructions, req.Prompt)
	}
	if req.System != "" {
		instructions = append(instructions, req.System)
	}
	if len(instructions) > 0 {
		systemPrompt += "\n\n" + strings.Join(instructions, "\n\n")
	}
	prefix := ""
	if req.UpstreamNativeTools {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": systemPrompt,
		})
	} else {
		prefix = systemPrompt + "\n\nUser request:\n"
	}

	switch input := req.Input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": prefix + input,
		})
	case []interface{}:
		var inputMessages []map[string]interface{}
		for _, item := range input {
			inputMessages = append(inputMessages, responseInputItemToChatMessages(item, req.UpstreamNativeTools)...)
		}
		if prefix != "" {
			inputMessages = prependToFirstUserMessage(inputMessages, prefix)
		}
		messages = append(messages, inputMessages...)
	default:
		if prefix != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": prefix,
			})
		}
	}

	return messages
}

func prependToFirstUserMessage(messages []map[string]interface{}, prefix string) []map[string]interface{} {
	if len(messages) == 0 {
		return []map[string]interface{}{{
			"role":    "user",
			"content": prefix,
		}}
	}
	for _, msg := range messages {
		if msg["role"] != "user" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = prefix + content
		default:
			msg["content"] = prefix + stringifyJSONValue(content)
		}
		return messages
	}
	return append([]map[string]interface{}{{
		"role":    "user",
		"content": prefix,
	}}, messages...)
}

func responseInputItemToChatMessages(item interface{}, nativeTools bool) []map[string]interface{} {
	itemMap, ok := item.(map[string]interface{})
	if !ok {
		return nil
	}

	itemType, _ := itemMap["type"].(string)
	role, _ := itemMap["role"].(string)
	switch itemType {
	case "", "message":
		if role == "" {
			role = "user"
		}
		return []map[string]interface{}{{
			"role":    normalizeChatRole(role, nativeTools),
			"content": responsesContentToChatContent(itemMap["content"]),
		}}
	case "function_call":
		callID := firstString(itemMap, "call_id", "id")
		name, _ := itemMap["name"].(string)
		if !nativeTools {
			return []map[string]interface{}{{
				"role": "assistant",
				"content": fmt.Sprintf("<action>{\"name\":%q,\"arguments\":%s}</action>",
					name, argumentsJSONString(itemMap["arguments"])),
			}}
		}
		return []map[string]interface{}{{
			"role":    "assistant",
			"content": "",
			"tool_calls": []interface{}{map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": stringifyJSONValue(itemMap["arguments"]),
				},
			}},
		}}
	case "function_call_output":
		if !nativeTools {
			return []map[string]interface{}{{
				"role": "user",
				"content": fmt.Sprintf("<observation call_id=%q>%s</observation>",
					firstString(itemMap, "call_id", "id"), stringifyJSONValue(itemMap["output"])),
			}}
		}
		return []map[string]interface{}{{
			"role":         "tool",
			"tool_call_id": firstString(itemMap, "call_id", "id"),
			"content":      stringifyJSONValue(itemMap["output"]),
		}}

	case "tool_use":
		callID := firstString(itemMap, "id", "call_id")
		name, _ := itemMap["name"].(string)
		inputMap, _ := itemMap["input"].(map[string]interface{})
		if !nativeTools {
			return []map[string]interface{}{{
				"role": "assistant",
				"content": fmt.Sprintf(`<action>{"name":%q,"arguments":%s}</action>`,
					name, argumentsJSONString(inputMap)),
			}}
		}
		return []map[string]interface{}{{
			"role":    "assistant",
			"content": "",
			"tool_calls": []interface{}{map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": stringifyJSONValue(inputMap),
				},
			}},
		}}

	case "tool_result":
		callID := firstString(itemMap, "call_id", "id")
		content := stringifyJSONValue(itemMap["content"])
		if !nativeTools {
			return []map[string]interface{}{{
				"role": "user",
				"content": fmt.Sprintf(`<observation call_id=%q>%s</observation>`, callID, content),
			}}
		}
		return []map[string]interface{}{{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      content,
		}}

	case "image":
		return []map[string]interface{}{{
			"role":    "user",
			"content": "[image content]",
		}}

	case "document":
		name := firstString(itemMap, "name")
		if name == "" {
			name = "document"
		}
		return []map[string]interface{}{{
			"role":    "user",
			"content": fmt.Sprintf("[document: %s]", name),
		}}

	default:
		if role == "" {
			return nil
		}
		return []map[string]interface{}{{
			"role":    normalizeChatRole(role, nativeTools),
			"content": responsesContentToChatContent(itemMap["content"]),
		}}
	}
}

func normalizeChatRole(role string, nativeTools bool) string {
	if role == "developer" || role == "system" {
		if !nativeTools {
			return "user"
		}
		return "system"
	}
	return role
}

func responsesContentToChatContent(content interface{}) interface{} {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []interface{}:
		chatParts := make([]interface{}, 0, len(c))
		var textParts []string
		hasNonText := false
		for _, raw := range c {
			block, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			text := blockText(block)
			switch blockType {
			case "input_text", "output_text", "text":
				if text != "" {
					textParts = append(textParts, text)
					chatParts = append(chatParts, map[string]interface{}{"type": "text", "text": text})
				}
			case "input_image":
				hasNonText = true
				if imageURL, ok := block["image_url"].(string); ok && imageURL != "" {
					chatParts = append(chatParts, map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": imageURL},
					})
				} else {
					chatParts = append(chatParts, block)
				}
			default:
				if text != "" {
					textParts = append(textParts, text)
					chatParts = append(chatParts, map[string]interface{}{"type": "text", "text": text})
				} else {
					hasNonText = true
					chatParts = append(chatParts, block)
				}
			}
		}
		if hasNonText {
			return chatParts
		}
		return strings.Join(textParts, "")
	default:
		return stringifyJSONValue(c)
	}
}

func buildUpstreamRequest(req *model.ResponsesRequest, messages []map[string]interface{}) map[string]interface{} {
	upstreamReq := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
	}
	if req.MaxOutput > 0 {
		upstreamReq["max_tokens"] = req.MaxOutput
	}
	if rawHas(req, "temperature") || req.Temperature != 0 {
		upstreamReq["temperature"] = req.Temperature
	}
	if rawHas(req, "top_p") || req.TopP != 0 {
		upstreamReq["top_p"] = req.TopP
	}
	if req.TopK != nil {
		upstreamReq["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		upstreamReq["stop_sequences"] = req.StopSequences
	}
	if rawHas(req, "parallel_tool_calls") {
		upstreamReq["parallel_tool_calls"] = req.ParallelToolCalls
	}
	if req.Text != nil && req.Text.Format != nil {
		if responseFormat := textFormatToChatResponseFormat(req.Text.Format); responseFormat != nil {
			upstreamReq["response_format"] = responseFormat
		}
	}
	return upstreamReq
}

func textFormatToChatResponseFormat(format *model.TextFormat) map[string]interface{} {
	switch format.Type {
	case "json_object":
		return map[string]interface{}{"type": "json_object"}
	case "json_schema":
		jsonSchema := map[string]interface{}{
			"name":   format.Name,
			"schema": format.Schema,
		}
		if format.Strict != nil {
			jsonSchema["strict"] = *format.Strict
		}
		return map[string]interface{}{
			"type":        "json_schema",
			"json_schema": jsonSchema,
		}
	default:
		return nil
	}
}

func formatToolListFromRequest(req *model.ResponsesRequest) string {
	if len(req.Tools) == 0 {
		return "(no tools available)"
	}
	var list []string
	for _, t := range req.Tools {
		name := t.Name
		if name == "" {
			name = t.Type
		}
		desc := t.Description
		if desc == "" {
			desc = "No description"
		}
		desc = compactDescription(desc, maxToolDescriptionRunes)
		line := fmt.Sprintf("- %s: %s", name, desc)
		if len(t.Parameters) > 0 {
			if args := compactParameterSummary(t.Parameters); args != "" {
				line += "\n  args: " + args
			}
		}
		list = append(list, line)
	}
	return strings.Join(list, "\n")
}

func compactDescription(desc string, maxRunes int) string {
	text := strings.Join(strings.Fields(desc), " ")
	if text == "" || maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

func compactParameterSummary(parameters map[string]interface{}) string {
	if len(parameters) == 0 {
		return ""
	}
	required := requiredParameterSet(parameters["required"])
	props, _ := parameters["properties"].(map[string]interface{})
	if len(props) == 0 {
		if len(required) == 0 {
			return schemaTypeName(parameters["type"])
		}
		names := sortedSetKeys(required)
		return "required: " + strings.Join(names, ", ")
	}

	parts := make([]string, 0, len(props))
	for name, raw := range props {
		prop, _ := raw.(map[string]interface{})
		part := name + ":" + schemaTypeName(prop["type"])
		if required[name] {
			part += " required"
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func requiredParameterSet(raw interface{}) map[string]bool {
	required := map[string]bool{}
	switch values := raw.(type) {
	case []interface{}:
		for _, value := range values {
			if name, ok := value.(string); ok && name != "" {
				required[name] = true
			}
		}
	case []string:
		for _, name := range values {
			if name != "" {
				required[name] = true
			}
		}
	}
	return required
}

func sortedSetKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func schemaTypeName(raw interface{}) string {
	switch value := raw.(type) {
	case string:
		if value != "" {
			return value
		}
	case []interface{}:
		var parts []string
		for _, item := range value {
			if s, ok := item.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "|")
		}
	}
	return "any"
}

func toolChoiceInstruction(choice interface{}) string {
	switch value := choice.(type) {
	case string:
		switch strings.ToLower(value) {
		case "required":
			return "Tool choice: call exactly one available tool."
		case "none":
			return "Tool choice: do not call tools."
		}
	case map[string]interface{}:
		if name, _ := value["name"].(string); name != "" {
			return fmt.Sprintf("Tool choice: call the %s tool.", name)
		}
		if fn, ok := value["function"].(map[string]interface{}); ok {
			if name, _ := fn["name"].(string); name != "" {
				return fmt.Sprintf("Tool choice: call the %s tool.", name)
			}
		}
	}
	return ""
}

func requestToolNames(reqTools []model.Tool) []string {
	names := make([]string, 0, len(reqTools))
	for _, t := range reqTools {
		if t.Name != "" {
			names = append(names, t.Name)
			continue
		}
		if t.Type != "" {
			names = append(names, t.Type)
		}
	}
	sort.Strings(names)
	return names
}

func convertRequestToolsToOpenAI(req *model.ResponsesRequest) []map[string]interface{} {
	if len(req.Tools) == 0 {
		return nil
	}
	var toolDefs []map[string]interface{}
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		fn := map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
		}
		if len(t.Parameters) > 0 {
			fn["parameters"] = t.Parameters
		} else {
			fn["parameters"] = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		toolDefs = append(toolDefs, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return toolDefs
}

func convertToolChoiceToOpenAIChat(choice interface{}) interface{} {
	choiceMap, ok := choice.(map[string]interface{})
	if !ok {
		return choice
	}
	choiceType, _ := choiceMap["type"].(string)
	name, _ := choiceMap["name"].(string)
	if choiceType == "function" && name != "" {
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": name},
		}
	}
	return choice
}

func buildUsage(req *model.ResponsesRequest, items []model.OutputItem) *model.UsageInfo {
	outTokens := countTokens(items)
	usage := &model.UsageInfo{
		InputTokens:  0,
		OutputTokens: outTokens,
		TotalTokens:  outTokens,
	}
	if req.IsThinkingEnabled() {
		usage.OutputTokensDetails = &model.OutputTokensDetail{
			ReasoningTokens: outTokens * 60 / 100,
		}
	}
	return usage
}

func responseID(req *model.ResponsesRequest, fallbackPrefix string) string {
	if req.ResponseID != "" {
		return req.ResponseID
	}
	return fmt.Sprintf("%s_%d", fallbackPrefix, time.Now().UnixNano())
}

func countTokens(items []model.OutputItem) int {
	var total int
	for _, item := range items {
		if item.Status == "completed" {
			total++
		}
		for _, block := range item.Content {
			n := len(block.Text)
			if len(block.Content) > n {
				n = len(block.Content)
			}
			total += n / 4
		}
	}
	return total
}

func firstChoiceMessage(resp map[string]interface{}) (map[string]interface{}, error) {
	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("invalid upstream response: no choices")
	}
	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid upstream response: malformed choice")
	}
	message, ok := firstChoice["message"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid upstream response: malformed message")
	}
	return message, nil
}

func parseChatToolCall(toolCall map[string]interface{}) (string, string, string) {
	callID, _ := toolCall["id"].(string)
	function, _ := toolCall["function"].(map[string]interface{})
	toolName, _ := function["name"].(string)
	argsStr, _ := function["arguments"].(string)
	if argsStr == "" {
		argsStr = "{}"
	}
	return callID, toolName, argsStr
}

func parseArguments(argsStr string) map[string]interface{} {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil || args == nil {
		return map[string]interface{}{}
	}
	return args
}

func mergeStreamingToolCalls(delta map[string]interface{}, currentToolCall *map[string]interface{}, toolCalls *[]map[string]interface{}) {
	rawToolCalls, exists := delta["tool_calls"]
	if !exists {
		return
	}
	tcList, ok := rawToolCalls.([]interface{})
	if !ok {
		return
	}
	for _, raw := range tcList {
		tMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if id, ok := tMap["id"].(string); ok && id != "" {
			if *currentToolCall != nil {
				*toolCalls = append(*toolCalls, *currentToolCall)
			}
			*currentToolCall = tMap
			continue
		}
		if *currentToolCall == nil {
			continue
		}
		mergeToolCallDelta(*currentToolCall, tMap)
	}
}

func mergeToolCallDelta(current map[string]interface{}, delta map[string]interface{}) {
	fnDelta, ok := delta["function"].(map[string]interface{})
	if !ok {
		return
	}
	fnCurrent, ok := current["function"].(map[string]interface{})
	if !ok {
		fnCurrent = map[string]interface{}{}
		current["function"] = fnCurrent
	}
	if name, ok := fnDelta["name"].(string); ok && name != "" {
		fnCurrent["name"] = name
	}
	if args, ok := fnDelta["arguments"].(string); ok {
		existingArgs, _ := fnCurrent["arguments"].(string)
		fnCurrent["arguments"] = existingArgs + args
	}
}

func parseDeltaFromSSEData(data string) (map[string]interface{}, error) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return nil, err
	}
	choices, ok := event["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil, nil
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	return delta, nil
}

func processUpstreamJSON(data []byte, currentToolCall *map[string]interface{}, toolCalls *[]map[string]interface{}, contentBuilder *strings.Builder, onEvent func(*model.SSEEvent) error) {
	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}
	choices, ok := event["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		// Non-streaming response format: message.content instead of delta.content
		message, ok := choice["message"].(map[string]interface{})
		if !ok {
			return
		}
		if content, ok := message["content"].(string); ok && content != "" {
			contentBuilder.WriteString(content)
			if onEvent != nil {
				_ = onEvent(&model.SSEEvent{
					Type: "response.output_text.delta",
					Data: map[string]interface{}{"delta": content},
				})
			}
		}
		rawToolCalls, ok := message["tool_calls"].([]interface{})
		if ok && len(rawToolCalls) > 0 {
			mergeStreamingToolCalls(map[string]interface{}{"tool_calls": rawToolCalls}, currentToolCall, toolCalls)
		}
		return
	}

	mergeStreamingToolCalls(delta, currentToolCall, toolCalls)

	if content, ok := delta["content"].(string); ok && content != "" {
		contentBuilder.WriteString(content)
		if onEvent != nil {
			_ = onEvent(&model.SSEEvent{
				Type: "response.output_text.delta",
				Data: map[string]interface{}{"delta": content},
			})
		}
	}
}

func mapsToInterfaces(items []map[string]interface{}) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func blockText(block map[string]interface{}) string {
	if text, ok := block["text"].(string); ok {
		return text
	}
	if text, ok := block["content"].(string); ok {
		return text
	}
	return ""
}

func argumentsJSONString(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(v) == "" {
			return "{}"
		}
		if json.Valid([]byte(v)) {
			return v
		}
		data, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(data)
	default:
		data, err := json.Marshal(v)
		if err != nil || len(data) == 0 {
			return "{}"
		}
		return string(data)
	}
}

func stringifyJSONValue(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

func rawHas(req *model.ResponsesRequest, key string) bool {
	if req.RawBody == nil {
		return false
	}
	_, ok := req.RawBody[key]
	return ok
}
