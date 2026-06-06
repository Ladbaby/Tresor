package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

func TestCustomHeaderPlugin(t *testing.T) {
	config := map[string]interface{}{
		"headers": map[string]interface{}{
			"X-Custom":  "test-value",
			"X-Debug":   "true",
		},
	}

	p, err := NewCustomHeaderPlugin(config)
	if err != nil {
		t.Fatalf("create plugin: %v", err)
	}

	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	body := []byte(`{"test": true}`)
	ctx := &engine.PipelineContext{}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	if newReq.Header.Get("X-Custom") != "test-value" {
		t.Fatalf("expected X-Custom: test-value, got %q", newReq.Header.Get("X-Custom"))
	}
	if newReq.Header.Get("X-Debug") != "true" {
		t.Fatalf("expected X-Debug: true, got %q", newReq.Header.Get("X-Debug"))
	}
	if string(newBody) != string(body) {
		t.Fatal("body should not change")
	}
}

func TestOpenAI2Anthropic_TransformRequest(t *testing.T) {
	p := &OpenAI2Anthropic{}

	openAIReq := map[string]interface{}{
		"model":    "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful"},
			map[string]interface{}{"role": "user", "content": "Hello"},
		},
		"max_tokens":   100,
		"temperature":  0.7,
	}
	body, _ := json.Marshal(openAIReq)

	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")

	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{
			APIKey: "sk-ant-test",
		},
	}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	// Verify the URL path changed
	if !strings.Contains(newReq.URL.Path, "/v1/messages") {
		t.Fatalf("expected path /v1/messages, got %s", newReq.URL.Path)
	}

	// Verify the body was transformed
	var anthropicReq map[string]interface{}
	if err := json.Unmarshal(newBody, &anthropicReq); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	if anthropicReq["model"] != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %v", anthropicReq["model"])
	}

	if anthropicReq["system"] != "You are helpful" {
		t.Fatalf("expected system prompt, got %v", anthropicReq["system"])
	}

	if anthropicReq["max_tokens"] != float64(100) {
		t.Fatalf("expected max_tokens 100, got %v", anthropicReq["max_tokens"])
	}
}

func TestAnthropic2OpenAI_TransformRequest(t *testing.T) {
	p := &Anthropic2OpenAI{}

	anthropicReq := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Hello"},
		},
		"system": "Be concise",
	}
	body, _ := json.Marshal(anthropicReq)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))

	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{
			APIKey: "sk-openai-test",
		},
	}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	// Verify path changed
	if !strings.Contains(newReq.URL.Path, "/v1/chat/completions") {
		t.Fatalf("expected /v1/chat/completions, got %s", newReq.URL.Path)
	}

	// Verify transformed body
	var openAIReq map[string]interface{}
	if err := json.Unmarshal(newBody, &openAIReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if openAIReq["model"] != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %v", openAIReq["model"])
	}

	// Verify system prompt was extracted
	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(messages))
	}
}

func TestRegistry_ListPlugins(t *testing.T) {
	r := NewRegistry()
	plugins := r.ListPlugins()
	if len(plugins) != 4 {
		t.Fatalf("expected 4 plugins, got %d", len(plugins))
	}

	ids := make(map[string]bool)
	for _, p := range plugins {
		ids[p.ID] = true
	}
	if !ids["custom_header"] {
		t.Fatal("expected custom_header plugin")
	}
	if !ids["openai2anthropic"] {
		t.Fatal("expected openai2anthropic plugin")
	}
	if !ids["anthropic2openai"] {
		t.Fatal("expected anthropic2openai plugin")
	}
	if !ids["fix_anthropic_images"] {
		t.Fatal("expected fix_anthropic_images plugin")
	}
}

func TestOpenAI2Anthropic_ResponseNonStreaming(t *testing.T) {
	p := &OpenAI2Anthropic{}
	anthropicResp := map[string]interface{}{
		"id":      "msg_123",
		"model":   "claude-sonnet-4-20250514",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "Hello!"}},
		"usage":   map[string]interface{}{"input_tokens": 10, "output_tokens": 5},
		"stop_reason": "end_turn",
	}
	body, _ := json.Marshal(anthropicResp)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}

	var openAIResp map[string]interface{}
	if err := json.Unmarshal(newBody, &openAIResp); err != nil {
		t.Fatalf("unmarshal transformed: %v", err)
	}

	if openAIResp["object"] != "chat.completion" {
		t.Fatalf("expected object chat.completion, got %v", openAIResp["object"])
	}
	choices := openAIResp["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hello!" {
		t.Fatalf("expected 'Hello!', got %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Fatalf("expected role 'assistant', got %v", msg["role"])
	}
}

func TestAnthropic2OpenAI_ResponseNonStreaming(t *testing.T) {
	p := &Anthropic2OpenAI{}
	openAIResp := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"model":   "gpt-4o",
		"choices": []interface{}{map[string]interface{}{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "Hello back!"}, "finish_reason": "stop"}},
		"usage":   map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	body, _ := json.Marshal(openAIResp)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}

	var anthroResp map[string]interface{}
	if err := json.Unmarshal(newBody, &anthroResp); err != nil {
		t.Fatalf("unmarshal transformed: %v", err)
	}

	content := anthroResp["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	text := content[0].(map[string]interface{})["text"].(string)
	if text != "Hello back!" {
		t.Fatalf("expected 'Hello back!', got %v", text)
	}
}

func TestOpenAI2Anthropic_ResponseStreaming(t *testing.T) {
	p := &OpenAI2Anthropic{}
	// Simulate Anthropic SSE stream
	input := `event: message_start
data: {"type":"message_start","message":{"id":"msg_123","model":"claude-sonnet-4-20250514"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}

event: message_stop
data: {"type":"message_stop"}

`

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/event-stream")

	newBody, err := p.TransformResponse(resp, []byte(input), &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform streaming response: %v", err)
	}

	output := string(newBody)
	// Should see OpenAI chunk style output
	if !strings.Contains(output, "data: ") {
		t.Fatal("expected SSE data lines in output")
	}
	if !strings.Contains(output, "[DONE]") {
		t.Fatal("expected [DONE] marker")
	}
	// Should have role delta
	if !strings.Contains(output, `"role":"assistant"`) {
		t.Fatal("expected role delta in first chunk")
	}
	// Should have content
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected content 'Hello' in output")
	}
	if !strings.Contains(output, "world") {
		t.Fatal("expected content 'world' in output")
	}
	// Stop reason should be mapped
	if !strings.Contains(output, `"finish_reason":"stop"`) {
		t.Fatal("expected finish_reason stop (mapped from end_turn)")
	}
}

func TestAnthropic2OpenAI_ResponseStreaming(t *testing.T) {
	p := &Anthropic2OpenAI{}
	// Simulate OpenAI SSE stream
	input := `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/event-stream")

	newBody, err := p.TransformResponse(resp, []byte(input), &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform streaming response: %v", err)
	}

	output := string(newBody)
	// Should have Anthropic events
	if !strings.Contains(output, "event: message_start") {
		t.Fatal("expected message_start event", output)
	}
	if !strings.Contains(output, "event: content_block_delta") {
		t.Fatal("expected content_block_delta events")
	}
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected content 'Hello'")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Fatal("expected message_stop event")
	}
}
