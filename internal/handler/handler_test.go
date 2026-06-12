package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hexpilot/api-proxy/internal/config"
	"github.com/hexpilot/api-proxy/internal/model"
)

func TestResponseContextStoresResponsesItems(t *testing.T) {
	h := &Handler{responseStore: make(map[string][]interface{})}
	req := &model.ResponsesRequest{Input: "What is the capital of France?"}
	resp := &model.ResponsesResponse{
		ID: "resp_1",
		Output: []model.OutputItem{
			{
				Type:      "function_call",
				CallID:    "call_1",
				Name:      "search",
				Arguments: `{"query":"France capital"}`,
			},
			{
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []model.ContentBlock{
					{Type: "output_text", Text: "Paris."},
				},
			},
		},
	}

	h.storeResponseContext(resp.ID, req, resp)
	got, ok := h.loadResponseContext(resp.ID)
	if !ok {
		t.Fatalf("loadResponseContext(%q) ok = false, want true", resp.ID)
	}
	if len(got) != 3 {
		t.Fatalf("loadResponseContext(%q) len = %d, want 3", resp.ID, len(got))
	}
	first := asMap(t, got[0])
	if first["role"] != "user" || first["content"] != req.Input {
		t.Errorf("loadResponseContext(%q)[0] = %v, want original user message", resp.ID, first)
	}
	second := asMap(t, got[1])
	if second["type"] != "function_call" || second["call_id"] != "call_1" {
		t.Errorf("loadResponseContext(%q)[1] = %v, want function call item", resp.ID, second)
	}
	third := asMap(t, got[2])
	if third["type"] != "message" || third["role"] != "assistant" {
		t.Errorf("loadResponseContext(%q)[2] = %v, want assistant message item", resp.ID, third)
	}
}

func TestMergeResponseContextAppendsCurrentInput(t *testing.T) {
	previous := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "First"},
		map[string]interface{}{"type": "message", "role": "assistant", "content": "Answer"},
	}

	got := mergeResponseContext(previous, "Follow up?")
	gotItems, ok := got.([]interface{})
	if !ok {
		t.Fatalf("mergeResponseContext(%v, %q) = %T, want []interface{}", previous, "Follow up?", got)
	}
	if len(gotItems) != 3 {
		t.Fatalf("mergeResponseContext(%v, %q) len = %d, want 3", previous, "Follow up?", len(gotItems))
	}
	last := asMap(t, gotItems[2])
	if last["role"] != "user" || last["content"] != "Follow up?" {
		t.Errorf("mergeResponseContext(%v, %q) last = %v, want user follow up", previous, "Follow up?", last)
	}
}

func TestChatCompletionsStreamIncludesFinishReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"test-model",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"pong"},
				"finish_reason":"stop"
			}]
		}`))
	}))
	defer upstream.Close()

	h := New(&config.Config{
		UpstreamAPI:   upstream.URL,
		DefaultMode:   "react",
		DefaultModel:  "test-model",
		ServerPort:    "0",
		UpstreamToken: "",
	})
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("stream body = %s, want final finish_reason stop", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("stream body = %s, want [DONE]", got)
	}
}

func TestChatCompletionsToolsAreAdaptedWhenUpstreamHasNoNativeTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("upstream request included tools: %v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"test-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"<action>{\"name\":\"read_file\",\"arguments\":{\"path\":\"README.md\"}}</action>"
				},
				"finish_reason":"stop"
			}]
		}`))
	}))
	defer upstream.Close()

	h := New(&config.Config{
		UpstreamAPI:         upstream.URL,
		DefaultMode:         "react",
		DefaultModel:        "test-model",
		UpstreamNativeTools: false,
		ServerPort:          "0",
		UpstreamToken:       "",
	})
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"read README"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"read_file",
				"description":"Read file",
				"parameters":{"type":"object","properties":{"path":{"type":"string"}}}
			}
		}],
		"tool_choice":"auto"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if !strings.Contains(got, `"tool_calls"`) {
		t.Fatalf("response body = %s, want chat tool_calls", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("response body = %s, want finish_reason tool_calls", got)
	}
}

func asMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	got, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("asMap(%v) = %T, want map[string]interface{}", value, value)
	}
	return got
}
