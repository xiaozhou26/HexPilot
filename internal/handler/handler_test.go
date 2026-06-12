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

// fakeUpstreamText returns an httptest server that serves a simple text
// chat completion response, suitable for exercising the Anthropic stream path.
func fakeUpstreamText(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}]
		}`))
	}))
}

// fakeUpstreamToolCalls returns an httptest server that serves a response with
// a single tool_call, useful for testing tool_use block emission.
func fakeUpstreamToolCalls(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"test-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"search","arguments":"{\"query\":\"test\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}]
		}`))
	}))
}

// setupHandlerForAnthropic builds a Handler wired to the given upstream URL.
func setupHandlerForAnthropic(upstreamURL string) *Handler {
	h := New(&config.Config{
		UpstreamAPI:   upstreamURL,
		DefaultMode:   "react",
		DefaultModel:  "test-model",
		ServerPort:    "0",
		UpstreamToken: "",
	})
	return h
}

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

func TestAnthropicToInternalParsesSystemArray(t *testing.T) {
	h := setupHandlerForAnthropic("http://example.com")
	raw := map[string]interface{}{
		"model":  "test-model",
		"stream": false,
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "You are a poet."},
			map[string]interface{}{"type": "text", "text": "Be concise."},
		},
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Hi"}},
	}
	req := h.anthropicToInternal(raw)
	if req.Instructions != "You are a poet.\n\nBe concise." {
		t.Errorf("Instructions = %q, want joined system blocks", req.Instructions)
	}
}

func TestAnthropicToInternalParsesToolChoice(t *testing.T) {
	h := setupHandlerForAnthropic("http://example.com")
	raw := map[string]interface{}{
		"model":       "test-model",
		"stream":      false,
		"tool_choice": map[string]interface{}{"type": "tool", "name": "search"},
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "Hi"}},
	}
	req := h.anthropicToInternal(raw)
	if req.ToolChoice == nil {
		t.Fatal("ToolChoice is nil, want parsed value")
	}
}

func TestAnthropicToInternalPreservesInputSchema(t *testing.T) {
	h := setupHandlerForAnthropic("http://example.com")
	raw := map[string]interface{}{
		"model": "test-model",
		"tools": []interface{}{
			map[string]interface{}{
				"name":        "search",
				"description": "Search",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Hi"}},
	}
	req := h.anthropicToInternal(raw)
	if len(req.Tools) != 1 {
		t.Fatalf("len(req.Tools) = %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Parameters == nil {
		t.Fatal("Tool.Parameters is nil, want input_schema preserved")
	}
}

func TestHandleAnthropicStreamEmitsFullSSESequence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := fakeUpstreamText(t, "Hello!")
	defer upstream.Close()

	h := setupHandlerForAnthropic(upstream.URL)
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Say hello"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "dummy")
	req.Header.Set("anthropic-version", "2023-06-01")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()

	required := []string{
		`event: message_start`,
		`"id":"msg_`,
		`"model":"test-model"`,
		`"input_tokens":0`,
		`event: content_block_start`,
		`"type":"text"`,
		`event: content_block_delta`,
		`"type":"text_delta"`,
		`"text":"Hello!"`,
		`event: content_block_stop`,
		`event: message_delta`,
		`"stop_reason":"end_turn"`,
		`event: message_stop`,
	}
	for _, token := range required {
		if !strings.Contains(got, token) {
			t.Errorf("SSE output missing %q\nFull output:\n%s", token, got)
		}
	}
}

func TestHandleAnthropicStreamEmitsToolUseBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := fakeUpstreamToolCalls(t)
	defer upstream.Close()

	h := setupHandlerForAnthropic(upstream.URL)
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model","max_tokens":64,"stream":true,
		"tools":[{"name":"search","description":"Search","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"Search for weather"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()

	required := []string{
		`event: message_start`,
		`event: content_block_start`,
		`"type":"tool_use"`,
		`"name":"search"`,
		`event: content_block_delta`,
		`"type":"input_json_delta"`,
		`event: content_block_stop`,
		`event: message_delta`,
		`"stop_reason":"tool_use"`,
		`event: message_stop`,
	}
	for _, token := range required {
		if !strings.Contains(got, token) {
			t.Errorf("SSE output missing %q\nFull output:\n%s", token, got)
		}
	}
}

func TestHandleAnthropicSyncIncludesToolUseBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := fakeUpstreamToolCalls(t)
	defer upstream.Close()

	h := setupHandlerForAnthropic(upstream.URL)
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model","max_tokens":64,"stream":false,
		"tools":[{"name":"search","description":"Search"}],
		"messages":[{"role":"user","content":"Search for weather"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()

	if !strings.Contains(got, `"type":"tool_use"`) {
		t.Errorf("sync response = %s, want tool_use content block", got)
	}
	if !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Errorf("sync response = %s, want stop_reason tool_use", got)
	}
	if !strings.Contains(got, `"name":"search"`) {
		t.Errorf("sync response = %s, want tool name search", got)
	}
}

func TestAnthropicStreamUsesCorrectBlockIndices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Upstream returns text first, then tool_calls (simulated by a response
	// that contains both content and tool_calls). We use a special upstream
	// that returns both.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"test-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"Here is the result.",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"search","arguments":"{\"query\":\"test\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}]
		}`))
	}))
	defer upstream.Close()

	h := setupHandlerForAnthropic(upstream.URL)
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model","max_tokens":64,"stream":true,
		"tools":[{"name":"search","description":"Search","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"Search for weather"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()

	// When both text and tool_use are present, text is at index 0,
	// tool_use at index 1.
	if !strings.Contains(got, `"index":0`) {
		t.Error("expected block index 0 for text block")
	}
	if !strings.Contains(got, `"index":1`) {
		t.Error("expected block index 1 for tool_use block")
	}
}

func TestGetTopKAndGetStringSliceHelpers(t *testing.T) {
	m := map[string]interface{}{
		"top_k":          float64(50),
		"stop_sequences": []interface{}{"\n", "END"},
	}

	topK := getTopK(m)
	if topK == nil || *topK != 50 {
		t.Errorf("getTopK = %v, want 50", topK)
	}

	seqs := getStringSlice(m, "stop_sequences")
	if len(seqs) != 2 || seqs[0] != "\n" || seqs[1] != "END" {
		t.Errorf("getStringSlice = %v, want [\\n END]", seqs)
	}

	// nil key
	if getTopK(map[string]interface{}{}) != nil {
		t.Error("getTopK should return nil for missing key")
	}
	if getStringSlice(map[string]interface{}{}, "stop") != nil {
		t.Error("getStringSlice should return nil for missing key")
	}
}

func TestAnthropicSyncStoresResponseContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := fakeUpstreamText(t, "Paris")
	defer upstream.Close()

	h := setupHandlerForAnthropic(upstream.URL)
	router := gin.New()
	h.RegisterRoutes(router)

	body := strings.NewReader(`{
		"model":"test-model","max_tokens":64,"stream":false,
		"messages":[{"role":"user","content":"What is the capital of France?"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Verify response context is stored — the response store should have the data
	h.storeMu.RLock()
	storeLen := len(h.responseStore)
	responsesLen := len(h.responses)
	h.storeMu.RUnlock()
	if storeLen == 0 {
		t.Error("responseStore should have entries after sync response")
	}
	if responsesLen == 0 {
		t.Error("responses map should have entries after sync response")
	}
}
