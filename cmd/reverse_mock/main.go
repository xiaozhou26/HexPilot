package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	// 模拟逆向 API — 标准 Chat Completions，不支持 tool calling
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		model, _ := req["model"].(string)
		isStream := false
		if s, ok := req["stream"].(bool); ok && s {
			isStream = true
		}
		isReasoning := false
		// 模拟深度思考：检查 system prompt 中是否有思考标签
		if msgs, ok := req["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if msg, ok := m.(map[string]interface{}); ok {
					if content, ok := msg["content"].(string); ok {
						if contains(content, "<thought>") {
							isReasoning = true
						}
					}
				}
			}
		}

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			reply := "Hello! This response comes from the reverse API through HexPilot's proxy."
			words := splitWords(reply)

			for _, word := range words {
				event := map[string]interface{}{
					"choices": []interface{}{
						map[string]interface{}{
							"delta": map[string]interface{}{"content": word + " "},
							"index": 0,
						},
					},
					"model": model,
				}
				data, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\n", data)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}

		// 非流式响应
		content := "Hello! I am the reverse-engineered API. I cannot call tools myself, but HexPilot handles tool calling for me."
		if isReasoning {
			content = "After deep analysis: the answer is 42. This demonstrates HexPilot's reasoning capability on top of an API that doesn't support it natively."
		}

		msg := map[string]interface{}{"role": "assistant", "content": content}
		resp := map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl_reverse_%d", len(content)),
			"object":  "chat.completion",
			"model":   model,
			"choices": []interface{}{map[string]interface{}{"index": 0, "message": msg, "finish_reason": "stop"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	addr := ":9999"
	fmt.Printf("Mock Reverse API (no tool calling support) running on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitWords(s string) []string {
	var words []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if i > start {
				words = append(words, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		words = append(words, s[start:])
	}
	return words
}
