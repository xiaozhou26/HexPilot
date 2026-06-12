package react

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexpilot/api-proxy/internal/config"
	"github.com/hexpilot/api-proxy/internal/model"
	"github.com/hexpilot/api-proxy/internal/proxy"
	"github.com/hexpilot/api-proxy/internal/tools"
)

func TestBuildInitialMessagesResponsesItems(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "Hello"},
				map[string]interface{}{"type": "input_text", "text": "!"},
			},
		},
		map[string]interface{}{
			"type": "message",
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "output_text", "text": "Hi"},
			},
		},
		map[string]interface{}{
			"type":      "function_call",
			"call_id":   "call_1",
			"name":      "search",
			"arguments": map[string]interface{}{"query": "weather"},
		},
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "sunny",
		},
	}
	req := &model.ResponsesRequest{
		Input:               input,
		Instructions:        "Be concise.",
		UpstreamNativeTools: true,
	}

	got := buildInitialMessages(req, "(no tools available)")
	if len(got) != 5 {
		t.Fatalf("buildInitialMessages(%v) len = %d, want 5", input, len(got))
	}
	if got[1]["role"] != "user" || got[1]["content"] != "Hello!" {
		t.Errorf("buildInitialMessages(%v)[1] = %v, want user Hello!", input, got[1])
	}
	if got[2]["role"] != "assistant" || got[2]["content"] != "Hi" {
		t.Errorf("buildInitialMessages(%v)[2] = %v, want assistant Hi", input, got[2])
	}

	toolCallMsg := got[3]
	if toolCallMsg["role"] != "assistant" {
		t.Fatalf("buildInitialMessages(%v)[3].role = %v, want assistant", input, toolCallMsg["role"])
	}
	toolCalls, ok := toolCallMsg["tool_calls"].([]interface{})
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("buildInitialMessages(%v)[3].tool_calls = %v, want one tool call", input, toolCallMsg["tool_calls"])
	}
	call := asMap(t, toolCalls[0])
	if call["id"] != "call_1" {
		t.Errorf("buildInitialMessages(%v) function call id = %v, want call_1", input, call["id"])
	}
	fn := asMap(t, call["function"])
	if fn["name"] != "search" || fn["arguments"] != `{"query":"weather"}` {
		t.Errorf("buildInitialMessages(%v) function = %v, want search with query args", input, fn)
	}

	if got[4]["role"] != "tool" || got[4]["tool_call_id"] != "call_1" || got[4]["content"] != "sunny" {
		t.Errorf("buildInitialMessages(%v)[4] = %v, want tool output for call_1", input, got[4])
	}
}

func TestBuildInitialMessagesGenericUpstreamAvoidsSystemRole(t *testing.T) {
	req := &model.ResponsesRequest{
		Input:               "ping",
		Instructions:        "Be concise.",
		UpstreamNativeTools: false,
	}

	got := buildInitialMessages(req, "(no tools available)")
	if len(got) != 1 {
		t.Fatalf("buildInitialMessages(%v) len = %d, want 1", req, len(got))
	}
	if got[0]["role"] != "user" {
		t.Errorf("buildInitialMessages(%v)[0].role = %v, want user", req, got[0]["role"])
	}
	content, ok := got[0]["content"].(string)
	if !ok || !strings.Contains(content, "Be concise.") || !strings.Contains(content, "ping") {
		t.Errorf("buildInitialMessages(%v)[0].content = %v, want prompt and user input", req, got[0]["content"])
	}
}

func TestBuildInitialMessagesGenericUpstreamConvertsFunctionItemsToText(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type":      "function_call",
			"call_id":   "call_1",
			"name":      "read_file",
			"arguments": map[string]interface{}{"path": "README.md"},
		},
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "file text",
		},
	}
	req := &model.ResponsesRequest{
		Input:               input,
		UpstreamNativeTools: false,
	}

	got := buildInitialMessages(req, "(no tools available)")
	if len(got) != 2 {
		t.Fatalf("buildInitialMessages(%v) len = %d, want 2", input, len(got))
	}
	if got[0]["role"] != "assistant" || !strings.Contains(got[0]["content"].(string), "<action>") {
		t.Errorf("buildInitialMessages(%v)[0] = %v, want assistant action text", input, got[0])
	}
	if got[1]["role"] != "user" || !strings.Contains(got[1]["content"].(string), "<observation") {
		t.Errorf("buildInitialMessages(%v)[1] = %v, want user observation text", input, got[1])
	}
}

func TestProcessReturnsFunctionCallForRequestTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %s, want /v1/chat/completions", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("upstream request unexpectedly included native tools: %v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
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
	defer server.Close()

	engine := NewEngine(proxy.New(&config.Config{UpstreamAPI: server.URL}), tools.NewRegistry())
	req := &model.ResponsesRequest{
		Model:      "test-model",
		Input:      "read it",
		ResponseID: "resp_test",
		Tools: []model.Tool{
			{
				Type:        "function",
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		},
	}

	got, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process() error = %v, want nil", err)
	}
	if got.ID != "resp_test" {
		t.Errorf("Process() ID = %q, want resp_test", got.ID)
	}
	if len(got.Output) != 1 {
		t.Fatalf("Process() output len = %d, want 1", len(got.Output))
	}
	call := got.Output[0]
	if call.Type != "function_call" || call.Name != "read_file" {
		t.Fatalf("Process() output[0] = %+v, want read_file function_call", call)
	}
	if call.Arguments != `{"path":"README.md"}` {
		t.Errorf("Process() arguments = %v, want README path JSON", call.Arguments)
	}
}

func TestProcessAcceptsToolCallsFromGenericUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("upstream request unexpectedly included native tools: %v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"",
					"tool_calls":[{
						"id":"call_shell",
						"type":"function",
						"function":{"name":"shell_command","arguments":"{\"command\":\"New-Item -Path test.txt -ItemType File\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}]
		}`))
	}))
	defer server.Close()

	engine := NewEngine(proxy.New(&config.Config{UpstreamAPI: server.URL}), tools.NewRegistry())
	req := &model.ResponsesRequest{
		Model:               "test-model",
		Input:               "create a txt file",
		ResponseID:          "resp_test",
		UpstreamNativeTools: false,
		Tools: []model.Tool{
			{
				Type:        "function",
				Name:        "shell_command",
				Description: "Run PowerShell.",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		},
	}

	got, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process() error = %v, want nil", err)
	}
	if len(got.Output) != 1 {
		t.Fatalf("Process() output len = %d, want 1", len(got.Output))
	}
	call := got.Output[0]
	if call.Type != "function_call" || call.Name != "shell_command" {
		t.Fatalf("Process() output[0] = %+v, want shell_command function_call", call)
	}
	if call.CallID != "call_shell" {
		t.Errorf("Process() call_id = %q, want call_shell", call.CallID)
	}
}

func TestProcessRetriesEmptyToolResponse(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-empty",
				"object":"chat.completion",
				"choices":[{
					"index":0,
					"message":{"role":"assistant","content":""},
					"finish_reason":"stop"
				}]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-action",
			"object":"chat.completion",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"<action>{\"name\":\"shell_command\",\"arguments\":{\"command\":\"New-Item -Path test.txt -ItemType File\"}}</action>"
				},
				"finish_reason":"stop"
			}]
		}`))
	}))
	defer server.Close()

	engine := NewEngine(proxy.New(&config.Config{UpstreamAPI: server.URL}), tools.NewRegistry())
	req := &model.ResponsesRequest{
		Model:      "test-model",
		Input:      "create a txt file",
		ResponseID: "resp_test",
		Tools: []model.Tool{
			{
				Type:        "function",
				Name:        "shell_command",
				Description: "Run PowerShell.",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		},
	}

	got, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process() error = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("Process() upstream calls = %d, want 2", calls)
	}
	if len(got.Output) != 1 || got.Output[0].Name != "shell_command" {
		t.Fatalf("Process() output = %+v, want shell_command function call", got.Output)
	}
}

func TestFormatToolListFromRequestCompactsParameters(t *testing.T) {
	req := &model.ResponsesRequest{
		Tools: []model.Tool{
			{
				Type:        "function",
				Name:        "shell_command",
				Description: strings.Repeat("Runs a Powershell command with a lot of details. ", 20),
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command":    map[string]interface{}{"type": "string"},
						"workdir":    map[string]interface{}{"type": "string"},
						"timeout_ms": map[string]interface{}{"type": "number"},
					},
					"required": []interface{}{"command"},
				},
			},
		},
	}

	got := formatToolListFromRequest(req)
	if len([]rune(got)) > 360 {
		t.Fatalf("formatToolListFromRequest(%v) len = %d, want <= 360; text = %q", req.Tools, len([]rune(got)), got)
	}
	if strings.Contains(got, `"properties"`) {
		t.Fatalf("formatToolListFromRequest(%v) = %q, want compact summary without raw schema JSON", req.Tools, got)
	}
	if !strings.Contains(got, "command:string required") {
		t.Fatalf("formatToolListFromRequest(%v) = %q, want required command arg", req.Tools, got)
	}
}

func TestConvertRequestToolsToOpenAIResponsesFunctionShape(t *testing.T) {
	strict := true
	req := &model.ResponsesRequest{
		Tools: []model.Tool{
			{
				Type:        "function",
				Name:        "get_weather",
				Description: "Get weather.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"location"},
				},
				Strict: &strict,
			},
		},
	}

	got := convertRequestToolsToOpenAI(req)
	if len(got) != 1 {
		t.Fatalf("convertRequestToolsToOpenAI(%v) len = %d, want 1", req.Tools, len(got))
	}
	if got[0]["type"] != "function" {
		t.Errorf("convertRequestToolsToOpenAI(%v)[0].type = %v, want function", req.Tools, got[0]["type"])
	}
	fn := asMap(t, got[0]["function"])
	if fn["name"] != "get_weather" || fn["strict"] != true {
		t.Errorf("convertRequestToolsToOpenAI(%v) function = %v, want strict get_weather", req.Tools, fn)
	}
	parameters := asMap(t, fn["parameters"])
	if parameters["type"] != "object" {
		t.Errorf("convertRequestToolsToOpenAI(%v) parameters.type = %v, want object", req.Tools, parameters["type"])
	}
}

func TestBuildUpstreamRequestMapsResponsesTextFormat(t *testing.T) {
	strict := true
	req := &model.ResponsesRequest{
		Model:             "gpt-test",
		MaxOutput:         128,
		Temperature:       0,
		ParallelToolCalls: false,
		RawBody: map[string]interface{}{
			"temperature":         float64(0),
			"parallel_tool_calls": false,
		},
		Text: &model.TextConfig{
			Format: &model.TextFormat{
				Type:   "json_schema",
				Name:   "weather",
				Schema: map[string]interface{}{"type": "object"},
				Strict: &strict,
			},
		},
	}
	messages := []map[string]interface{}{{"role": "user", "content": "ping"}}

	got := buildUpstreamRequest(req, messages)
	if got["max_tokens"] != 128 {
		t.Errorf("buildUpstreamRequest(%v).max_tokens = %v, want 128", req, got["max_tokens"])
	}
	if got["temperature"] != float64(0) {
		t.Errorf("buildUpstreamRequest(%v).temperature = %v, want 0", req, got["temperature"])
	}
	if got["parallel_tool_calls"] != false {
		t.Errorf("buildUpstreamRequest(%v).parallel_tool_calls = %v, want false", req, got["parallel_tool_calls"])
	}
	responseFormat := asMap(t, got["response_format"])
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("buildUpstreamRequest(%v).response_format = %v, want json_schema", req, responseFormat)
	}
	jsonSchema := asMap(t, responseFormat["json_schema"])
	if jsonSchema["name"] != "weather" || jsonSchema["strict"] != true {
		t.Errorf("buildUpstreamRequest(%v).json_schema = %v, want strict weather schema", req, jsonSchema)
	}
}

func TestParseActionFromTaggedContent(t *testing.T) {
	input := `<thought>Need search.</thought><action>{"name":"search","arguments":{"query":"weather"}}</action>`

	got, ok := parseAction(input)
	if !ok {
		t.Fatalf("parseAction(%q) ok = false, want true", input)
	}
	if got.Name != "search" {
		t.Errorf("parseAction(%q).Name = %q, want search", input, got.Name)
	}
	if got.Arguments["query"] != "weather" {
		t.Errorf("parseAction(%q).Arguments[query] = %v, want weather", input, got.Arguments["query"])
	}
}

func TestExtractFinalAnswer(t *testing.T) {
	input := `<thought>Done.</thought><final_answer>Hello.</final_answer>`

	got := extractFinalAnswer(input)
	if got != "Hello." {
		t.Errorf("extractFinalAnswer(%q) = %q, want Hello.", input, got)
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
