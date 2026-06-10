package proxy

import (
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

// ForwardRequest 转发请求到上游 API（非流式）
func (p *Proxy) ForwardRequest(reqBody map[string]interface{}) (map[string]interface{}, error) {
	// 确保非流式请求
	reqBody["stream"] = false

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Debug: 打印完整请求体
	log.Printf("[UPSTREAM REQ] %s", string(bodyBytes))

	req, err := http.NewRequest("POST", p.config.UpstreamAPI+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
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

	if resp.StatusCode != http.StatusOK {
		// 检测是否返回了 HTML 页面（常见于反向代理配置错误）
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") || strings.HasPrefix(strings.TrimSpace(string(respBody)), "<!DOCTYPE") {
			return nil, fmt.Errorf("upstream returned HTML instead of JSON (status %d). This usually means UPSTREAM_API is pointing to a web UI, not an API endpoint. Check your .env file. Current UPSTREAM_API=%s", resp.StatusCode, p.config.UpstreamAPI)
		}
		return nil, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(respBody))
	}

	// 检测响应内容类型
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") || strings.HasPrefix(strings.TrimSpace(string(respBody)), "<!DOCTYPE") {
		return nil, fmt.Errorf("upstream returned HTML instead of JSON (status 200). This usually means UPSTREAM_API is pointing to a web UI, not an API endpoint. Current UPSTREAM_API=%s, full URL=%s", p.config.UpstreamAPI, p.config.UpstreamAPI+"/v1/chat/completions")
	}

	// Debug: 打印原始响应
	log.Printf("Upstream response (first 200 bytes): %s", string(respBody[:min(200, len(respBody))]))

	// 检测是否是 SSE 流格式（上游可能忽略 stream=false 仍返回流式）
	if strings.HasPrefix(string(respBody), "data:") || strings.HasPrefix(string(respBody), "event:") || strings.Contains(string(respBody), "\ndata:") {
		// 解析 SSE 流，提取最终回复
		return parseSSEToResponse(respBody)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (raw: %s)", err, string(respBody[:min(200, len(respBody))]))
	}

	return result, nil
}

// parseSSEToResponse 将上游 SSE 流解析为标准 OpenAI chat completion 响应
func parseSSEToResponse(sseData []byte) (map[string]interface{}, error) {
	var contentBuilder strings.Builder
	var role string
	var model string
	var finishReason string

	lines := strings.Split(string(sseData), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// 提取 model
		if m, ok := chunk["model"].(string); ok && m != "" {
			model = m
		}

		// 提取 choices
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		first := choices[0].(map[string]interface{})

		// delta content
		if delta, ok := first["delta"].(map[string]interface{}); ok {
			if r, ok := delta["role"].(string); ok {
				role = r
			}
			if c, ok := delta["content"].(string); ok {
				contentBuilder.WriteString(c)
			}
		}

		// finish_reason
		if fr, ok := first["finish_reason"].(string); ok && fr != "null" {
			finishReason = fr
		}
	}

	if role == "" {
		role = "assistant"
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	// 构建标准 OpenAI chat completion 响应
	result := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", len(sseData)),
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    role,
					"content": contentBuilder.String(),
				},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}

	return result, nil
}

// ForwardRequestStream 转发请求到上游 API（流式）
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
	req.Header.Set("Accept", "text/event-stream")
	if p.config.UpstreamToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.UpstreamToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(body))
	}

	// 逐行读取 SSE 流
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if cbErr := onChunk(chunk); cbErr != nil {
				return cbErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read stream: %w", err)
		}
	}

	return nil
}
