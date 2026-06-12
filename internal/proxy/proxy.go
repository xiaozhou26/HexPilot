package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/hexpilot/api-proxy/internal/config"
)

type Proxy struct {
	config *config.Config
	client *http.Client
}

func New(cfg *config.Config) *Proxy {
	return &Proxy{
		config: cfg,
		client: &http.Client{},
	}
}

func (p *Proxy) ForwardRequest(reqBody map[string]interface{}) (map[string]interface{}, error) {
	reqBody["stream"] = false

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	log.Printf("[UPSTREAM REQ] %s", string(bodyBytes))

	req, err := http.NewRequest("POST", p.config.UpstreamAPI+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream, text/plain")
	if p.config.UpstreamToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.UpstreamToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK {
		if looksLikeHTML(contentType, respBody) {
			return nil, fmt.Errorf("upstream returned HTML instead of API data (status %d). UPSTREAM_API may point to a web UI or the upstream adapter returned an HTML page. Current UPSTREAM_API=%s", resp.StatusCode, p.config.UpstreamAPI)
		}
		return nil, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(respBody))
	}
	if looksLikeHTML(contentType, respBody) {
		return nil, fmt.Errorf("upstream returned HTML instead of API data (status 200). Current UPSTREAM_API=%s, full URL=%s. Test that URL directly; if it is an adapter like aichat, make sure the adapter itself is not receiving an HTML page from its remote upstream", p.config.UpstreamAPI, p.config.UpstreamAPI+"/v1/chat/completions")
	}

	log.Printf("Upstream response (first 1000 bytes): %s", string(respBody[:min(1000, len(respBody))]))

	if isSSE(contentType, respBody) {
		return parseSSEToResponse(respBody)
	}
	if !strings.Contains(contentType, "application/json") {
		return wrapTextAsChatCompletion(respBody, reqBody), nil
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (raw: %s)", err, string(respBody[:min(200, len(respBody))]))
	}
	if err := checkEmptyResponse(result, respBody); err != nil {
		return nil, err
	}
	return result, nil
}

func parseSSEToResponse(sseData []byte) (map[string]interface{}, error) {
	var contentBuilder strings.Builder
	var role string
	var modelName string
	var finishReason string
	var toolCalls []map[string]interface{}

	scanner := bufio.NewScanner(bytes.NewReader(sseData))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		if strings.HasPrefix(data, " ") {
			data = data[1:]
		}
		control := strings.TrimSpace(data)
		if control == "" || control == "[DONE]" {
			if control == "[DONE]" {
				break
			}
			continue
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			contentBuilder.WriteString(data)
			continue
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			modelName = m
		}
		appendChunkText(&contentBuilder, &role, &finishReason, &toolCalls, chunk)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan SSE: %w", err)
	}
	if role == "" {
		role = "assistant"
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	return chatCompletionResponseWithToolCalls(fmt.Sprintf("chatcmpl-%d", len(sseData)), modelName, role, contentBuilder.String(), finishReason, toolCalls), nil
}

func appendChunkText(contentBuilder *strings.Builder, role *string, finishReason *string, toolCalls *[]map[string]interface{}, chunk map[string]interface{}) {
	choices, ok := chunk["choices"].([]interface{})
	if ok && len(choices) > 0 {
		first, ok := choices[0].(map[string]interface{})
		if !ok {
			return
		}
		if delta, ok := first["delta"].(map[string]interface{}); ok {
			if r, ok := delta["role"].(string); ok && r != "" {
				*role = r
			}
			if c, ok := delta["content"].(string); ok {
				contentBuilder.WriteString(c)
			}
			if rawToolCalls, ok := delta["tool_calls"].([]interface{}); ok {
				mergeStreamingToolCalls(rawToolCalls, toolCalls)
			}
		}
		if message, ok := first["message"].(map[string]interface{}); ok {
			if r, ok := message["role"].(string); ok && r != "" {
				*role = r
			}
			if c, ok := message["content"].(string); ok {
				contentBuilder.WriteString(c)
			}
			if rawToolCalls, ok := message["tool_calls"].([]interface{}); ok {
				*toolCalls = normalizeToolCalls(rawToolCalls)
			}
		}
		if fr, ok := first["finish_reason"].(string); ok && fr != "" && fr != "null" {
			*finishReason = fr
		}
		return
	}

	for _, key := range []string{"content", "text", "delta", "message"} {
		if text, ok := chunk[key].(string); ok {
			contentBuilder.WriteString(text)
			return
		}
	}
}

func wrapTextAsChatCompletion(body []byte, reqBody map[string]interface{}) map[string]interface{} {
	modelName, _ := reqBody["model"].(string)
	return chatCompletionResponse(
		fmt.Sprintf("chatcmpl-text-%d", len(body)),
		modelName,
		"assistant",
		strings.TrimSpace(string(body)),
		"stop",
	)
}

func chatCompletionResponse(id, modelName, role, content, finishReason string) map[string]interface{} {
	return chatCompletionResponseWithToolCalls(id, modelName, role, content, finishReason, nil)
}

func chatCompletionResponseWithToolCalls(id, modelName, role, content, finishReason string, toolCalls []map[string]interface{}) map[string]interface{} {
	message := map[string]interface{}{
		"role":    role,
		"content": content,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = mapsToInterfaces(toolCalls)
		if finishReason == "" || finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}
	return map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": 0,
		"model":   modelName,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
}

func mergeStreamingToolCalls(rawToolCalls []interface{}, toolCalls *[]map[string]interface{}) {
	for _, raw := range rawToolCalls {
		delta, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		index := intFromNumber(delta["index"], len(*toolCalls))
		for len(*toolCalls) <= index {
			*toolCalls = append(*toolCalls, map[string]interface{}{})
		}
		current := (*toolCalls)[index]
		if id, ok := delta["id"].(string); ok && id != "" {
			current["id"] = id
		}
		if typ, ok := delta["type"].(string); ok && typ != "" {
			current["type"] = typ
		} else if current["type"] == nil {
			current["type"] = "function"
		}
		if fnDelta, ok := delta["function"].(map[string]interface{}); ok {
			mergeFunctionDelta(current, fnDelta)
		}
	}
}

func mergeFunctionDelta(current map[string]interface{}, fnDelta map[string]interface{}) {
	fnCurrent, ok := current["function"].(map[string]interface{})
	if !ok {
		fnCurrent = map[string]interface{}{}
		current["function"] = fnCurrent
	}
	if name, ok := fnDelta["name"].(string); ok && name != "" {
		fnCurrent["name"] = name
	}
	if args, ok := fnDelta["arguments"].(string); ok {
		existing, _ := fnCurrent["arguments"].(string)
		fnCurrent["arguments"] = existing + args
	}
}

func normalizeToolCalls(rawToolCalls []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rawToolCalls))
	for _, raw := range rawToolCalls {
		toolCall, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := toolCall["type"].(string); !ok {
			toolCall["type"] = "function"
		}
		out = append(out, toolCall)
	}
	return out
}

func mapsToInterfaces(items []map[string]interface{}) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func intFromNumber(value interface{}, fallback int) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return fallback
	}
}

func (p *Proxy) ForwardRequestStream(reqBody map[string]interface{}, onChunk func([]byte) error) error {
	reqBody["stream"] = true

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", p.config.UpstreamAPI+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json, text/plain")
	if p.config.UpstreamToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.UpstreamToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(respBody))
	}

	// Accumulate the full body before processing so that non-SSE (plain JSON)
	// responses are parsed as complete objects rather than partial chunks.
	var fullBody bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			fullBody.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read stream: %w", err)
		}
	}
	body := fullBody.Bytes()

	if isSSE(resp.Header.Get("Content-Type"), body) {
		// SSE path: emit each data line individually.
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			if cbErr := onChunk([]byte(data)); cbErr != nil {
				return cbErr
			}
		}
	} else {
		// Non-SSE path: pass the complete body as a single chunk so the
		// callback can parse it as a full JSON object.
		if cbErr := onChunk(body); cbErr != nil {
			return cbErr
		}
	}
	return nil
}

func isSSE(contentType string, body []byte) bool {
	bodyText := string(body)
	return strings.Contains(contentType, "text/event-stream") ||
		strings.HasPrefix(bodyText, "data:") ||
		strings.HasPrefix(bodyText, "event:") ||
		strings.Contains(bodyText, "\ndata:")
}

func checkEmptyResponse(result map[string]interface{}, rawBody []byte) error {
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return fmt.Errorf("upstream returned empty choices (raw: %s)", string(rawBody[:min(200, len(rawBody))]))
	}
	first, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil
	}
	message, ok := first["message"].(map[string]interface{})
	if !ok {
		return nil
	}
	content, _ := message["content"].(string)
	toolCalls, hasToolCalls := message["tool_calls"]
	finishReason, _ := first["finish_reason"].(string)
	if content == "" && !hasToolCalls && (finishReason == "" || finishReason == "null") {
		if toolCalls != nil {
			if tc, ok := toolCalls.([]interface{}); ok && len(tc) > 0 {
				return nil
			}
		}
		return fmt.Errorf("upstream returned empty content with no tool_calls (raw: %s)", string(rawBody[:min(300, len(rawBody))]))
	}
	return nil
}

func looksLikeHTML(contentType string, body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.Contains(contentType, "text/html") ||
		strings.HasPrefix(trimmed, "<!DOCTYPE") ||
		strings.HasPrefix(trimmed, "<html") ||
		strings.HasPrefix(trimmed, "<HTML")
}
