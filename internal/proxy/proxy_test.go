package proxy

import "testing"

func TestParseSSEToResponseOpenAIChunks(t *testing.T) {
	input := []byte(`: OPENROUTER PROCESSING

data: {"model":"deepseek/test","choices":[{"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

data: {"model":"deepseek/test","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}

data: [DONE]
`)

	got, err := parseSSEToResponse(input)
	if err != nil {
		t.Fatalf("parseSSEToResponse(%q) error = %v, want nil", input, err)
	}
	choices := got["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if message["content"] != "Hello" {
		t.Errorf("parseSSEToResponse(%q) content = %q, want Hello", input, message["content"])
	}
	if got["model"] != "deepseek/test" {
		t.Errorf("parseSSEToResponse(%q) model = %q, want deepseek/test", input, got["model"])
	}
}

func TestParseSSEToResponseTextData(t *testing.T) {
	input := []byte("data: hello\n\ndata:  world\n\n")

	got, err := parseSSEToResponse(input)
	if err != nil {
		t.Fatalf("parseSSEToResponse(%q) error = %v, want nil", input, err)
	}
	choices := got["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if message["content"] != "hello world" {
		t.Errorf("parseSSEToResponse(%q) content = %q, want hello world", input, message["content"])
	}
}

func TestParseSSEToResponseToolCallChunks(t *testing.T) {
	input := []byte(`data: {"model":"deepseek/test","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell_command","arguments":"{\"command\":"}}]},"finish_reason":null}]}

data: {"model":"deepseek/test","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Get-ChildItem\"}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]
`)

	got, err := parseSSEToResponse(input)
	if err != nil {
		t.Fatalf("parseSSEToResponse(%q) error = %v, want nil", input, err)
	}
	choices := got["choices"].([]interface{})
	first := choices[0].(map[string]interface{})
	if first["finish_reason"] != "tool_calls" {
		t.Fatalf("parseSSEToResponse(%q) finish_reason = %q, want tool_calls", input, first["finish_reason"])
	}
	message := first["message"].(map[string]interface{})
	rawToolCalls, ok := message["tool_calls"].([]interface{})
	if !ok || len(rawToolCalls) != 1 {
		t.Fatalf("parseSSEToResponse(%q) tool_calls = %v, want one", input, message["tool_calls"])
	}
	toolCall := rawToolCalls[0].(map[string]interface{})
	fn := toolCall["function"].(map[string]interface{})
	if fn["name"] != "shell_command" {
		t.Errorf("parseSSEToResponse(%q) function.name = %q, want shell_command", input, fn["name"])
	}
	if fn["arguments"] != `{"command":"Get-ChildItem"}` {
		t.Errorf("parseSSEToResponse(%q) function.arguments = %q, want command JSON", input, fn["arguments"])
	}
}

func TestWrapTextAsChatCompletion(t *testing.T) {
	input := []byte(" plain answer ")
	req := map[string]interface{}{"model": "plain-model"}

	got := wrapTextAsChatCompletion(input, req)
	choices := got["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if message["content"] != "plain answer" {
		t.Errorf("wrapTextAsChatCompletion(%q, %v) content = %q, want plain answer", input, req, message["content"])
	}
	if got["model"] != "plain-model" {
		t.Errorf("wrapTextAsChatCompletion(%q, %v) model = %q, want plain-model", input, req, got["model"])
	}
}
