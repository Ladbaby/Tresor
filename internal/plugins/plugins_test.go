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
			"X-Custom": "test-value",
			"X-Debug":  "true",
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

func TestOpenAI2Anthropic_TransformRequest_Basic(t *testing.T) {
	p := &OpenAI2Anthropic{}

	openAIReq := map[string]interface{}{
		"model":    "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful"},
			map[string]interface{}{"role": "user", "content": "Hello"},
		},
		"max_tokens":  100,
		"temperature": 0.7,
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

	if !strings.Contains(newReq.URL.Path, "/v1/messages") {
		t.Fatalf("expected path /v1/messages, got %s", newReq.URL.Path)
	}

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

func TestOpenAI2Anthropic_TransformRequest_Tools(t *testing.T) {
	p := &OpenAI2Anthropic{}

	openAIReq := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "What's the weather?"},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_weather",
					"description": "Get weather for a location",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{"type": "string"},
						},
						"required": []interface{}{"location"},
					},
				},
			},
		},
		"tool_choice": "auto",
		"stop":        []interface{}{"\n", "###"},
	}
	body, _ := json.Marshal(openAIReq)

	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{APIKey: "sk-ant-test"},
	}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var anthReq map[string]interface{}
	json.Unmarshal(newBody, &anthReq)

	// Verify tools converted (input_schema instead of parameters, no function wrapper)
	tools := anthReq["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "get_weather" {
		t.Fatalf("expected tool name get_weather, got %v", tool["name"])
	}
	if _, hasFn := tool["function"]; hasFn {
		t.Fatal("should not have function wrapper in Anthropic format")
	}
	if _, hasSchema := tool["input_schema"]; !hasSchema {
		t.Fatal("expected input_schema in Anthropic tool")
	}

	// Verify tool_choice
	tc := anthReq["tool_choice"].(map[string]interface{})
	if tc["type"] != "auto" {
		t.Fatalf("expected tool_choice type auto, got %v", tc["type"])
	}

	// Verify stop_sequences
	stops := anthReq["stop_sequences"].([]interface{})
	if len(stops) != 2 {
		t.Fatalf("expected 2 stop_sequences, got %d", len(stops))
	}
}

func TestOpenAI2Anthropic_TransformRequest_ToolChoice_Object(t *testing.T) {
	p := &OpenAI2Anthropic{}

	// Object-style tool_choice: {"type":"function","function":{"name":"get_weather"}}
	openAIReq := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
		"tool_choice": map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "get_weather",
			},
		},
	}
	body, _ := json.Marshal(openAIReq)

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{APIKey: "sk-ant-test"},
	}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var anthReq map[string]interface{}
	json.Unmarshal(newBody, &anthReq)

	tc := anthReq["tool_choice"].(map[string]interface{})
	if tc["type"] != "tool" {
		t.Fatalf("expected tool_choice type tool, got %v", tc["type"])
	}
	if tc["name"] != "get_weather" {
		t.Fatalf("expected tool_choice name get_weather, got %v", tc["name"])
	}
}

func TestOpenAI2Anthropic_TransformRequest_ToolCallsInMessage(t *testing.T) {
	p := &OpenAI2Anthropic{}

	openAIReq := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "What's the weather in Paris?"},
			map[string]interface{}{
				"role":    "assistant",
				"content": "",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_123",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "get_weather",
							"arguments": `{"location":"Paris"}`,
						},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"content":      "22°C",
				"tool_call_id": "call_123",
			},
		},
	}
	body, _ := json.Marshal(openAIReq)

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{APIKey: "sk-ant-test"},
	}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var anthReq map[string]interface{}
	json.Unmarshal(newBody, &anthReq)

	messages := anthReq["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Assistant message should have content as blocks with tool_use
	assistantMsg := messages[1].(map[string]interface{})
	blocks := assistantMsg["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 content block for tool_use, got %d", len(blocks))
	}
	block := blocks[0].(map[string]interface{})
	if block["type"] != "tool_use" {
		t.Fatalf("expected block type tool_use, got %v", block["type"])
	}
	if block["id"] != "call_123" {
		t.Fatalf("expected tool_use id call_123, got %v", block["id"])
	}

	// Tool result message
	toolMsg := messages[2].(map[string]interface{})
	blocks2 := toolMsg["content"].([]interface{})
	if len(blocks2) != 1 {
		t.Fatalf("expected 1 content block for tool_result, got %d", len(blocks2))
	}
	block2 := blocks2[0].(map[string]interface{})
	if block2["type"] != "tool_result" {
		t.Fatalf("expected block type tool_result, got %v", block2["type"])
	}
}

func TestOpenAI2Anthropic_TransformResponse_NonStreaming_ToolUse(t *testing.T) {
	p := &OpenAI2Anthropic{}
	// Anthropic response with tool_use content block
	anthroResp := map[string]interface{}{
		"id":    "msg_123",
		"model": "claude-sonnet-4-20250514",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Let me check..."},
			map[string]interface{}{"type": "tool_use", "id": "toolu_abc", "name": "get_weather", "input": map[string]interface{}{"location": "Paris"}},
		},
		"usage":       map[string]interface{}{"input_tokens": 10, "output_tokens": 20},
		"stop_reason": "tool_use",
	}
	body, _ := json.Marshal(anthroResp)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}

	var openAIResp map[string]interface{}
	json.Unmarshal(newBody, &openAIResp)

	choices := openAIResp["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})

	if msg["content"] != "Let me check..." {
		t.Fatalf("expected text content, got %v", msg["content"])
	}

	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "toolu_abc" {
		t.Fatalf("expected tool_call id toolu_abc, got %v", tc["id"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("expected function name get_weather, got %v", fn["name"])
	}
}

func TestOpenAI2Anthropic_ResponseStreaming_ToolUse(t *testing.T) {
	p := &OpenAI2Anthropic{}
	input := `event: message_start
data: {"type":"message_start","message":{"id":"msg_123","model":"claude-sonnet-4-20250514"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"I'll"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" look that up"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}

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
	if !strings.Contains(output, `"role":"assistant"`) {
		t.Fatal("expected role assistant")
	}
	if !strings.Contains(output, "I'll") {
		t.Fatal("expected text content")
	}
	// Should have tool_calls in chunks for tool_use blocks
	if !strings.Contains(output, "tool_calls") {
		t.Fatal("expected tool_calls in output for tool_use block")
	}
	if !strings.Contains(output, "get_weather") {
		t.Fatal("expected tool name get_weather")
	}
	// Anthropic input_json_delta converts to OpenAI tool_calls with arguments fragments
	if !strings.Contains(output, `"arguments"`) {
		t.Fatal("expected arguments in output for input_json_delta")
	}
	// Stop reason should be mapped
	if !strings.Contains(output, `"finish_reason":"tool_calls"`) {
		t.Fatal("expected finish_reason tool_calls (mapped from tool_use)")
	}
}

func TestOpenAI2Anthropic_TransformStreamChunk_ToolUse(t *testing.T) {
	p := &OpenAI2Anthropic{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Message start
	chunk1 := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-20250514"}}`),
	}
	result1, err := p.TransformStreamChunk(chunk1, ctx)
	if err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if !strings.Contains(string(result1.Data), `"role":"assistant"`) {
		t.Fatal("expected role assistant in message_start chunk")
	}

	// Text content block start
	chunk2 := engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hello"}}`),
	}
	result2, err := p.TransformStreamChunk(chunk2, ctx)
	if err != nil {
		t.Fatalf("content_block_start text: %v", err)
	}
	if !strings.Contains(string(result2.Data), "Hello") {
		t.Fatal("expected Hello in content")
	}

	// Tool_use content block start
	chunk3 := engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"get_weather"}}`),
	}
	result3, err := p.TransformStreamChunk(chunk3, ctx)
	if err != nil {
		t.Fatalf("content_block_start tool_use: %v", err)
	}
	if !strings.Contains(string(result3.Data), "tool_calls") {
		t.Fatal("expected tool_calls in output for tool_use block")
	}
	if !strings.Contains(string(result3.Data), "get_weather") {
		t.Fatal("expected tool name get_weather")
	}

	// Input JSON delta
	chunk4 := engine.SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"loc\":\"Paris\"}"}}`),
	}
	result4, err := p.TransformStreamChunk(chunk4, ctx)
	if err != nil {
		t.Fatalf("input_json_delta: %v", err)
	}
	if !strings.Contains(string(result4.Data), `"arguments"`) {
		t.Fatal("expected arguments in input_json_delta output")
	}
}

// ----- Anthropic2OpenAI request tests -----

func TestAnthropic2OpenAI_TransformRequest_Basic(t *testing.T) {
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

	if !strings.Contains(newReq.URL.Path, "/v1/chat/completions") {
		t.Fatalf("expected /v1/chat/completions, got %s", newReq.URL.Path)
	}

	var openAIReq map[string]interface{}
	if err := json.Unmarshal(newBody, &openAIReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if openAIReq["model"] != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %v", openAIReq["model"])
	}

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(messages))
	}
}

func TestAnthropic2OpenAI_TransformRequest_Tools(t *testing.T) {
	p := &Anthropic2OpenAI{}

	anthropicReq := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Weather?"},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"name":        "get_weather",
				"description": "Get weather data",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
		"tool_choice":    map[string]interface{}{"type": "auto"},
		"stop_sequences": []interface{}{"\n"},
	}
	body, _ := json.Marshal(anthropicReq)

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{APIKey: "sk-openai-test"},
	}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	// Tools converted
	tools := openAIReq["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" {
		t.Fatalf("expected tool type function, got %v", tool["type"])
	}
	fn := tool["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("expected function name get_weather, got %v", fn["name"])
	}
	if _, hasParams := fn["parameters"]; !hasParams {
		t.Fatal("expected parameters key in OpenAI tool")
	}

	// tool_choice
	if openAIReq["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice auto, got %v", openAIReq["tool_choice"])
	}

	// stop
	stops := openAIReq["stop"].([]interface{})
	if len(stops) != 1 {
		t.Fatalf("expected 1 stop, got %d", len(stops))
	}
}

func TestAnthropic2OpenAI_TransformRequest_ToolChoice_Any(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":       "claude-sonnet-4-20250514",
		"max_tokens":  100,
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"tool_choice": map[string]interface{}{"type": "any"},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)
	if openAIReq["tool_choice"] != "required" {
		t.Fatalf("expected tool_choice required (mapped from any), got %v", openAIReq["tool_choice"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_ToolChoice_Tool(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":       "claude-sonnet-4-20250514",
		"max_tokens":  100,
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"tool_choice": map[string]interface{}{"type": "tool", "name": "get_weather"},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)
	tc := openAIReq["tool_choice"].(map[string]interface{})
	if tc["type"] != "function" {
		t.Fatalf("expected tool_choice type function, got %v", tc["type"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_ToolUseContentBlocks(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// Message with tool_use and tool_result content blocks
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Weather in Paris?"},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Let me check..."},
					map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": map[string]interface{}{"location": "Paris"}},
				},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "tu_1", "content": "22°C"},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Assistant message should have tool_calls
	assistantMsg := messages[1].(map[string]interface{})
	tcs := assistantMsg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tu_1" {
		t.Fatalf("expected tool_call id tu_1, got %v", tc["id"])
	}

	// Text content should be preserved
	if assistantMsg["content"] != "Let me check..." {
		t.Fatalf("expected assistant content 'Let me check...', got %v", assistantMsg["content"])
	}

	// Tool result message
	toolMsg := messages[2].(map[string]interface{})
	if toolMsg["role"] != "tool" {
		t.Fatalf("expected tool role, got %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "tu_1" {
		t.Fatalf("expected tool_call_id tu_1, got %v", toolMsg["tool_call_id"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_ToolResultWithImage(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// Anthropic request with tool_result containing an image in inner content
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Show me a chart"},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Here is your chart:"},
					map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "render_chart", "input": map[string]interface{}{"data": []interface{}{1, 2, 3}}},
				},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tu_1",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "Chart rendered successfully"},
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "Y2hhcnQtZGF0YQ==",
								},
							},
						},
					},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Third message should be the tool role with multi-modal content array
	toolMsg := messages[2].(map[string]interface{})
	if toolMsg["role"] != "tool" {
		t.Fatalf("expected tool role, got %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "tu_1" {
		t.Fatalf("expected tool_call_id tu_1, got %v", toolMsg["tool_call_id"])
	}

	// Content should be an array (multi-modal), not a plain string
	contentArr, ok := toolMsg["content"].([]interface{})
	if !ok {
		t.Fatalf("expected content to be an array for multi-modal tool message, got type %T", toolMsg["content"])
	}
	if len(contentArr) != 2 {
		t.Fatalf("expected 2 content parts (text + image), got %d", len(contentArr))
	}

	// First part: text
	textPart := contentArr[0].(map[string]interface{})
	if textPart["type"] != "text" {
		t.Fatalf("expected first part type text, got %v", textPart["type"])
	}
	if textPart["text"] != "Chart rendered successfully" {
		t.Fatalf("expected text 'Chart rendered successfully', got %v", textPart["text"])
	}

	// Second part: image_url
	imgPart := contentArr[1].(map[string]interface{})
	if imgPart["type"] != "image_url" {
		t.Fatalf("expected second part type image_url, got %v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]interface{})
	url := imgURL["url"].(string)
	if url != "data:image/png;base64,Y2hhcnQtZGF0YQ==" {
		t.Fatalf("expected url 'data:image/png;base64,Y2hhcnQtZGF0YQ==', got %v", url)
	}
}

func TestAnthropic2OpenAI_TransformRequest_UserMessageWithImage(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// Anthropic request with user message containing text + image content blocks
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "What is in this image?"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/jpeg",
							"data":       "aW1hZ2UtZGF0YQ==",
						},
					},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	userMsg := messages[0].(map[string]interface{})
	if userMsg["role"] != "user" {
		t.Fatalf("expected user role, got %v", userMsg["role"])
	}

	// Content should be an array (multi-modal), not a plain string
	contentArr, ok := userMsg["content"].([]interface{})
	if !ok {
		t.Fatalf("expected content to be an array for multi-modal message, got type %T val %v", userMsg["content"], userMsg["content"])
	}
	if len(contentArr) != 2 {
		t.Fatalf("expected 2 content parts (text + image), got %d", len(contentArr))
	}

	// First part: text
	textPart := contentArr[0].(map[string]interface{})
	if textPart["type"] != "text" {
		t.Fatalf("expected first part type text, got %v", textPart["type"])
	}
	if textPart["text"] != "What is in this image?" {
		t.Fatalf("expected text 'What is in this image?', got %v", textPart["text"])
	}

	// Second part: image_url
	imgPart := contentArr[1].(map[string]interface{})
	if imgPart["type"] != "image_url" {
		t.Fatalf("expected second part type image_url, got %v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]interface{})
	url := imgURL["url"].(string)
	if url != "data:image/jpeg;base64,aW1hZ2UtZGF0YQ==" {
		t.Fatalf("expected url 'data:image/jpeg;base64,aW1hZ2UtZGF0YQ==', got %v", url)
	}
}

func TestAnthropic2OpenAI_TransformRequest_ImageUrlSource(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// Anthropic request with URL-sourced image
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "What is this?"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type": "url",
							"url":  "https://example.com/photo.png",
						},
					},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	userMsg := messages[0].(map[string]interface{})
	contentArr := userMsg["content"].([]interface{})
	imgPart := contentArr[1].(map[string]interface{})
	imgURL := imgPart["image_url"].(map[string]interface{})
	if imgURL["url"] != "https://example.com/photo.png" {
		t.Fatalf("expected url 'https://example.com/photo.png', got %v", imgURL["url"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_AssistantWithImage(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// Assistant message with both tool_use and image content blocks
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Draw a chart"},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Here you go:"},
					map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "draw_chart", "input": map[string]interface{}{"width": 400}},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "Y2hhcnQ=",
						},
					},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// Assistant message should have content as array (text + image), not plain string
	assistantMsg := messages[1].(map[string]interface{})
	contentArr, ok := assistantMsg["content"].([]interface{})
	if !ok {
		t.Fatalf("expected content to be array for assistant with images, got type %T", assistantMsg["content"])
	}

	// Should have text part and image part
	if len(contentArr) != 2 {
		t.Fatalf("expected 2 content parts (text + image), got %d", len(contentArr))
	}
	textPart := contentArr[0].(map[string]interface{})
	if textPart["type"] != "text" || textPart["text"] != "Here you go:" {
		t.Fatalf("unexpected text part: %v", textPart)
	}
	imgPart := contentArr[1].(map[string]interface{})
	if imgPart["type"] != "image_url" {
		t.Fatalf("expected image_url, got %v", imgPart["type"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_ThinkingBlock(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "text": "I need to think about this"},
					map[string]interface{}{"type": "text", "text": "Here is my answer"},
				},
			},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})

	// Content should only contain the text block, not thinking
	content, _ := msg["content"].(string)
	if content != "Here is my answer" {
		t.Fatalf("expected content 'Here is my answer', got %v", content)
	}

	// Thinking block should be in reasoning_content field
	reasoning, hasReasoning := msg["reasoning_content"]
	if !hasReasoning {
		t.Fatal("expected reasoning_content field for thinking block")
	}
	if reasoning != "I need to think about this" {
		t.Fatalf("expected reasoning_content 'I need to think about this', got %v", reasoning)
	}
}

func TestAnthropic2OpenAI_TransformRequest_SystemAsArray(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// System prompt as array of content blocks
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "Be concise."},
			map[string]interface{}{"type": "text", "text": "Use English."},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	messages := openAIReq["messages"].([]interface{})
	sysMsg := messages[0].(map[string]interface{})
	if sysMsg["role"] != "system" {
		t.Fatal("expected system message")
	}
	content := sysMsg["content"].(string)
	if !strings.Contains(content, "Be concise") || !strings.Contains(content, "Use English") {
		t.Fatalf("expected concatenated system prompts, got %v", content)
	}
}

// ----- Anthropic2OpenAI response tests -----

func TestAnthropic2OpenAI_ResponseNonStreaming_ToolCalls(t *testing.T) {
	p := &Anthropic2OpenAI{}

	openAIResp := map[string]interface{}{
		"id":     "chatcmpl-123",
		"object": "chat.completion",
		"model":  "gpt-4o",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Let me check...",
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id":   "call_1",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "get_weather",
								"arguments": `{"location":"Paris"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 15, "total_tokens": 25},
	}
	body, _ := json.Marshal(openAIResp)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}

	var anthroResp map[string]interface{}
	json.Unmarshal(newBody, &anthroResp)

	content := anthroResp["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (text + tool_use), got %d", len(content))
	}

	// First block: text
	textBlock := content[0].(map[string]interface{})
	if textBlock["type"] != "text" {
		t.Fatalf("expected first block type text, got %v", textBlock["type"])
	}

	// Second block: tool_use
	toolBlock := content[1].(map[string]interface{})
	if toolBlock["type"] != "tool_use" {
		t.Fatalf("expected second block type tool_use, got %v", toolBlock["type"])
	}
	if toolBlock["id"] != "call_1" {
		t.Fatalf("expected tool_use id call_1, got %v", toolBlock["id"])
	}
	if toolBlock["name"] != "get_weather" {
		t.Fatalf("expected tool_use name get_weather, got %v", toolBlock["name"])
	}
}

// ----- Truncation regression tests -----

func TestAnthropic2OpenAI_ResponseStreaming_TruncationRegression(t *testing.T) {
	p := &Anthropic2OpenAI{}
	// This was the exact bug: role chunk with empty content followed by text chunks.
	// The old code deferred content_block_start via pendingContentBlock, emitted an
	// empty data: \n\n event, and corrupted the stream. The new code always emits
	// content_block_start immediately with whatever content (including empty) on the
	// role chunk.
	input := `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"! How can I help"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/event-stream")

	newBody, err := p.TransformResponse(resp, []byte(input), &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform streaming: %v", err)
	}

	output := string(newBody)

	// The leading "Hi" MUST NOT be truncated to "! How can I help"
	if !strings.Contains(output, "Hi") {
		t.Fatal("TRUNCATION BUG: 'Hi' is missing from output. The leading characters were eaten!")
	}
	if !strings.Contains(output, "How can I help") {
		t.Fatal("expected rest of response in output")
	}

	// Must have content_block_start with empty text (the role chunk)
	if !strings.Contains(output, `"content_block_start"`) {
		t.Fatal("expected content_block_start event")
	}

	// Must NOT have empty data: lines between events (the old bug symptom)
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if line == "data: " {
			t.Fatalf("EMPTY DATA LINE at line %d — this was the root cause of the truncation bug", i+1)
		}
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_TruncationRegression(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// First chunk: role with empty content (the exact scenario that caused truncation)
	chunk1 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
	}
	result1, err := p.TransformStreamChunk(chunk1, ctx)
	if err != nil {
		t.Fatalf("chunk1: %v", err)
	}

	// Must emit content_block_start even for empty content
	if !strings.Contains(string(result1.Data), "content_block_start") {
		t.Fatal("should emit content_block_start on role chunk even with empty content")
	}
	// Must NOT emit empty data
	if strings.TrimSpace(string(result1.Data)) == "" {
		t.Fatal("should NOT return empty data on role chunk with empty content")
	}

	// Second chunk: actual text
	chunk2 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`),
	}
	result2, err := p.TransformStreamChunk(chunk2, ctx)
	if err != nil {
		t.Fatalf("chunk2: %v", err)
	}

	// Should be a content_block_delta with "Hi", NOT a content_block_start
	output2 := string(result2.Data)
	if !strings.Contains(output2, "Hi") {
		t.Fatal("expected 'Hi' in second chunk output")
	}
	if strings.Contains(output2, "content_block_start") {
		t.Fatal("second chunk should be delta, not start — first chunk already opened the block")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_ImmediateContentNoTruncation(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Role chunk WITH immediate content (also should work — regression guard)
	chunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}

	output := string(result.Data)
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected immediate content 'Hello'")
	}
	if !strings.Contains(output, "content_block_start") {
		t.Fatal("expected content_block_start for immediate content")
	}
}

// ----- Anthropic2OpenAI streaming tool call tests -----

func TestAnthropic2OpenAI_TransformStreamChunk_ToolCalls(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Chunk 1: message_start (implied by role in first chunk)
	chunk1 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
	}
	_, err := p.TransformStreamChunk(chunk1, ctx)
	if err != nil {
		t.Fatalf("chunk1: %v", err)
	}

	// Chunk 2: first tool_call with id, name, and first arguments fragment
	chunk2 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":"}}]},"finish_reason":null}]}`),
	}
	result2, err := p.TransformStreamChunk(chunk2, ctx)
	if err != nil {
		t.Fatalf("chunk2: %v", err)
	}

	output2 := string(result2.Data)
	if !strings.Contains(output2, "content_block_start") {
		t.Fatal("expected content_block_start for new tool call")
	}
	if !strings.Contains(output2, "tool_use") {
		t.Fatal("expected content block type tool_use")
	}
	if !strings.Contains(output2, "input_json_delta") {
		t.Fatal("expected input_json_delta for arguments")
	}

	// Chunk 3: tool call arguments continuation
	chunk3 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`),
	}
	result3, err := p.TransformStreamChunk(chunk3, ctx)
	if err != nil {
		t.Fatalf("chunk3: %v", err)
	}

	output3 := string(result3.Data)
	if !strings.Contains(output3, "input_json_delta") {
		t.Fatal("expected input_json_delta for arguments continuation")
	}
	if !strings.Contains(output3, `"partial_json"`) {
		t.Fatal("expected partial_json field in delta")
	}

	// Chunk 4: finish_reason
	chunk4 := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	result4, err := p.TransformStreamChunk(chunk4, ctx)
	if err != nil {
		t.Fatalf("chunk4: %v", err)
	}

	output4 := string(result4.Data)
	// Text block stop (was opened by chunk1)
	if !strings.Contains(output4, "content_block_stop") {
		t.Fatal("expected content_block_stop events on finish")
	}
	// Stop reason mapped
	if !strings.Contains(output4, "tool_use") {
		t.Fatal("expected stop_reason tool_use (mapped from tool_calls)")
	}
	if !strings.Contains(output4, "message_delta") {
		t.Fatal("expected message_delta on finish")
	}
}

func TestAnthropic2OpenAI_ResponseStreaming_ToolCalls(t *testing.T) {
	p := &Anthropic2OpenAI{}
	// The batch transformStreamingResponse path processes OpenAI SSE chunks but
	// does not track tool calls across chunks (that's handled by TransformStreamChunk).
	// This test verifies the batch path at least produces valid Anthropic SSE events
	// for the text content and passes finish_reason correctly.
	input := `data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/event-stream")

	newBody, err := p.TransformResponse(resp, []byte(input), &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform streaming: %v", err)
	}

	output := string(newBody)
	if !strings.Contains(output, "event: message_start") {
		t.Fatal("expected message_start event")
	}
	if !strings.Contains(output, "event: content_block_start") {
		t.Fatal("expected content_block_start")
	}
	// Stop reason tool_calls mapped to tool_use
	if !strings.Contains(output, "tool_use") {
		t.Fatal("expected stop_reason tool_use")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Fatal("expected message_stop event")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_DONE_Termination(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Send a role chunk first so state exists
	p.TransformStreamChunk(engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
	}, ctx)

	// DONE marker without finish_reason having been sent
	done := engine.SSEChunk{Data: []byte("[DONE]")}
	result, err := p.TransformStreamChunk(done, ctx)
	if err != nil {
		t.Fatalf("DONE: %v", err)
	}

	output := string(result.Data)
	if !strings.Contains(output, "message_delta") {
		t.Fatal("expected message_delta on DONE when not sent yet")
	}
	if !strings.Contains(output, "message_stop") {
		t.Fatal("expected message_stop on DONE")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_DONE_AlreadySentFinish(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Send a chunk with finish_reason so messageDeltaSent becomes true
	p.TransformStreamChunk(engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
	}, ctx)
	p.TransformStreamChunk(engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, ctx)

	// DONE marker after finish_reason already sent
	done := engine.SSEChunk{Data: []byte("[DONE]")}
	result, err := p.TransformStreamChunk(done, ctx)
	if err != nil {
		t.Fatalf("DONE: %v", err)
	}

	output := string(result.Data)
	if strings.Contains(output, "message_delta") {
		t.Fatal("should NOT emit message_delta when already sent")
	}
	if !strings.Contains(output, "message_stop") {
		t.Fatal("expected message_stop on DONE")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_FinishReason_Stop(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	p.TransformStreamChunk(engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
	}, ctx)

	chunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "end_turn") {
		t.Fatal("expected stop_reason end_turn (mapped from stop)")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_FinishReason_Length(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	p.TransformStreamChunk(engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
	}, ctx)

	chunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "max_tokens") {
		t.Fatal("expected stop_reason max_tokens (mapped from length)")
	}
}

// ----- Model mapping tests -----

func TestMapModel_Unknown(t *testing.T) {
	if mapModel("unknown-model") != "unknown-model" {
		t.Fatal("unknown model should pass through unchanged")
	}
}

func TestMapModelReverse_Unknown(t *testing.T) {
	if mapModelReverse("unknown-model") != "unknown-model" {
		t.Fatal("unknown model should pass through unchanged")
	}
}

func TestMapModel_KnownMappings(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"gpt-4o", "claude-sonnet-4-20250514"},
		{"gpt-4o-mini", "claude-haiku-3-5-20241022"},
		{"gpt-4-turbo", "claude-opus-4-20250514"},
	}
	for _, tt := range tests {
		got := mapModel(tt.input)
		if got != tt.expected {
			t.Errorf("mapModel(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMapModelReverse_KnownMappings(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"claude-sonnet-4-20250514", "gpt-4o"},
		{"claude-haiku-3-5-20241022", "gpt-4o-mini"},
		{"claude-opus-4-20250514", "gpt-4-turbo"},
	}
	for _, tt := range tests {
		got := mapModelReverse(tt.input)
		if got != tt.expected {
			t.Errorf("mapModelReverse(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ----- Edge case tests -----

func TestAnthropic2OpenAI_TransformRequest_EmptyMessages(t *testing.T) {
	p := &Anthropic2OpenAI{}
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 100,
		"messages":   []interface{}{},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)
	msgs := openAIReq["messages"].([]interface{})
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

func TestAnthropic2OpenAI_TransformRequest_NilContent(t *testing.T) {
	p := &Anthropic2OpenAI{}
	// Message with null/omitted content
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": nil},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)
	msgs := openAIReq["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// Content should be empty string, not null
	content, exists := msgs[0].(map[string]interface{})["content"]
	if !exists {
		t.Fatal("content field should exist")
	}
	if content != "" && content != nil {
		t.Fatalf("expected empty content, got %v", content)
	}
}

func TestAnthropic2OpenAI_TransformResponse_NonJSONPassthrough(t *testing.T) {
	p := &Anthropic2OpenAI{}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/plain")

	body := []byte("Not JSON - error page from downstream")
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(newBody) != string(body) {
		t.Fatal("non-JSON body should pass through unchanged")
	}
}

func TestOpenAI2Anthropic_TransformResponse_NonJSONPassthrough(t *testing.T) {
	p := &OpenAI2Anthropic{}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "text/plain")

	body := []byte("Not JSON")
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(newBody) != string(body) {
		t.Fatal("non-JSON body should pass through unchanged")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_EmptyContentBlock(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// content_block_start with empty text should still emit the event
	// (non-text blocks should be skipped)
	// Actually the key scenario: role with empty content emits empty-data that was the bug
	chunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}

	// Data must not be empty — this was the core truncation bug
	if len(result.Data) == 0 {
		t.Fatal("result data should NOT be empty — empty data caused the truncation bug")
	}

	// Must contain a valid SSE event
	if !strings.Contains(string(result.Data), "content_block_start") {
		t.Fatal("expected content_block_start event in output")
	}
}

func TestAnthropic2OpenAI_TransformStreamChunk_PassthroughInvalidJSON(t *testing.T) {
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Non-JSON data (not DONE marker either) should pass through
	chunk := engine.SSEChunk{Data: []byte("not json at all")}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if string(result.Data) != "not json at all" {
		t.Fatal("non-JSON data should pass through unchanged")
	}
}

// ----- OpenAI2Anthropic stream chunk tests -----

func TestOpenAI2Anthropic_TransformStreamChunk_ContentBlockStop(t *testing.T) {
	p := &OpenAI2Anthropic{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// content_block_stop should produce no output (it's a no-op for OpenAI format)
	chunk := engine.SSEChunk{
		EventType: "content_block_stop",
		Data:      []byte(`{"type":"content_block_stop","index":0}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("content_block_stop: %v", err)
	}
	// Should return empty (no output for stop events)
	if len(result.Data) != 0 {
		t.Fatal("content_block_stop should produce no output")
	}
}

// ----- Registry test -----

func TestRegistry_ListPlugins(t *testing.T) {
	r := NewRegistry()
	plugins := r.ListPlugins()
	if len(plugins) != 8 {
		t.Fatalf("expected 8 plugins, got %d", len(plugins))
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
		if !ids["openai2responses"] {
			t.Fatal("expected openai2responses plugin")
		}
		if !ids["anthropic2responses"] {
			t.Fatal("expected anthropic2responses plugin")
		}
		t.Fatal("expected fix_anthropic_images plugin")
	}
	if !ids["responses2openai"] {
		t.Fatal("expected responses2openai plugin")
	}
	if !ids["responses2anthropic"] {
		t.Fatal("expected responses2anthropic plugin")
	}
}

// ---- Existing tests preserved below ----

func TestOpenAI2Anthropic_TransformRequest_ContentBlocks(t *testing.T) {
	p := &Anthropic2OpenAI{}

	anthropicReq := map[string]interface{}{
		"model":    "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Hello"},
			}},
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Hi there"},
			}},
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "How are you?"},
			}},
		},
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
		t.Fatalf("transform with content blocks: %v", err)
	}

	if !strings.Contains(newReq.URL.Path, "/v1/chat/completions") {
		t.Fatalf("expected /v1/chat/completions, got %s", newReq.URL.Path)
	}

	var openAIReq map[string]interface{}
	if err := json.Unmarshal(newBody, &openAIReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	messages := openAIReq["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	userMsg := messages[0].(map[string]interface{})
	if userMsg["content"] != "Hello" {
		t.Fatalf("expected 'Hello', got %v", userMsg["content"])
	}

	assistantMsg := messages[1].(map[string]interface{})
	if assistantMsg["content"] != "Hi there" {
		t.Fatalf("expected 'Hi there', got %v", assistantMsg["content"])
	}
}

func TestAnthropic2OpenAI_TransformRequest_MixedContent(t *testing.T) {
	p := &Anthropic2OpenAI{}

	anthropicReq := map[string]interface{}{
		"model":    "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Hello"},
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Hi there"},
			}},
		},
	}
	body, _ := json.Marshal(anthropicReq)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{
			APIKey: "sk-openai-test",
		},
	}

	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform with mixed content: %v", err)
	}

	if !strings.Contains(newReq.URL.Path, "/v1/chat/completions") {
		t.Fatalf("expected /v1/chat/completions, got %s", newReq.URL.Path)
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
	if !strings.Contains(output, "data: ") {
		t.Fatal("expected SSE data lines in output")
	}
	if !strings.Contains(output, "[DONE]") {
		t.Fatal("expected [DONE] marker")
	}
	if !strings.Contains(output, `"role":"assistant"`) {
		t.Fatal("expected role delta in first chunk")
	}
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected content 'Hello' in output")
	}
	if !strings.Contains(output, "world") {
		t.Fatal("expected content 'world' in output")
	}
	if !strings.Contains(output, `"finish_reason":"stop"`) {
		t.Fatal("expected finish_reason stop (mapped from end_turn)")
	}
}

// ----- Responses2OpenAI tests -----

func TestResponses2OpenAI_TransformRequest_Basic(t *testing.T) {
	p := &Responses2OpenAI{}
	body := []byte(`{
		"model": "gpt-4o",
		"instructions": "Be helpful",
		"input": [{"role": "user", "content": "Hello"}],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	messages := result["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	sys := messages[0].(map[string]interface{})
	if sys["role"] != "system" || sys["content"] != "Be helpful" {
		t.Fatalf("expected system message 'Be helpful', got %v", sys)
	}
	user := messages[1].(map[string]interface{})
	if user["role"] != "user" || user["content"] != "Hello" {
		t.Fatalf("expected user message 'Hello', got %v", user)
	}
	if result["model"] != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %v", result["model"])
	}
	if newReq.URL.Path != "/v1/chat/completions" {
		t.Fatalf("expected path /v1/chat/completions, got %s", newReq.URL.Path)
	}
}

func TestResponses2OpenAI_TransformRequest_ToolCall(t *testing.T) {
	p := &Responses2OpenAI{}
	body := []byte(`{
		"model": "gpt-4o",
		"input": [
			{"role": "user", "content": "What's the weather?"},
			{"type": "function_call", "call_id": "call_123", "name": "get_weather", "arguments": "{\"city\":\"London\"}"},
			{"type": "function_call_output", "call_id": "call_123", "output": "Sunny"}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	messages := result["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	// Assistant with tool_calls
	assistant := messages[1].(map[string]interface{})
	if assistant["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %v", assistant["role"])
	}
	tcs := assistant["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	// Tool result
	tool := messages[2].(map[string]interface{})
	if tool["role"] != "tool" {
		t.Fatalf("expected tool role, got %v", tool["role"])
	}
	if tool["content"] != "Sunny" {
		t.Fatalf("expected tool content 'Sunny', got %v", tool["content"])
	}
}

func TestResponses2OpenAI_TransformRequest_Reasoning(t *testing.T) {
	p := &Responses2OpenAI{}
	body := []byte(`{
		"model": "gpt-4o",
		"input": [{"role": "user", "content": "Think hard"}],
		"reasoning": {"effort": "high"},
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if result["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort 'high', got %v", result["reasoning_effort"])
	}
}

func TestResponses2OpenAI_TransformResponse_NonStreaming(t *testing.T) {
	p := &Responses2OpenAI{}
	respBody := []byte(`{
		"id": "chatcmpl-123",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello world"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
	}`)
	httpResp := &http.Response{Header: http.Header{}}
	transformed, err := p.TransformResponse(httpResp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatal(err)
	}
	var result responsesResponse
	if err := json.Unmarshal(transformed, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result.Object != "response" {
		t.Fatalf("expected object 'response', got %s", result.Object)
	}
	if result.Status != "completed" {
		t.Fatalf("expected status 'completed', got %s", result.Status)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(result.Output))
	}
	if result.Output[0].Type != "output_text" {
		t.Fatalf("expected output_text type, got %s", result.Output[0].Type)
	}
	if result.Output[0].Text != "Hello world" {
		t.Fatalf("expected text 'Hello world', got %s", result.Output[0].Text)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 20 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
}

func TestResponses2OpenAI_TransformStreamChunk_FirstChunk(t *testing.T) {
	p := &Responses2OpenAI{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}
	chunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[]}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "response.created") {
		t.Fatal("expected response.created event, got:", output)
	}
	if !strings.Contains(output, "response.in_progress") {
		t.Fatal("expected response.in_progress event, got:", output)
	}
}

func TestResponses2OpenAI_TransformStreamChunk_Content(t *testing.T) {
	p := &Responses2OpenAI{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}
	// First call: role chunk to initialize state
	initChunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`),
	}
	p.TransformStreamChunk(initChunk, ctx)
	// Content delta
	contentChunk := engine.SSEChunk{
		Data: []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"}}]}`),
	}
	result, err := p.TransformStreamChunk(contentChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "response.output_text.delta") {
		t.Fatal("expected response.output_text.delta event, got:", output)
	}
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected content 'Hello', got:", output)
	}
}

// ----- Responses2Anthropic tests -----

func TestResponses2Anthropic_TransformRequest_Basic(t *testing.T) {
	p := &Responses2Anthropic{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"instructions": "Be helpful",
		"input": [{"role": "user", "content": "Hello"}],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if result["system"] != "Be helpful" {
		t.Fatalf("expected system 'Be helpful', got %v", result["system"])
	}
	messages := result["messages"].([]interface{})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	user := messages[0].(map[string]interface{})
	if user["role"] != "user" || user["content"] != "Hello" {
		t.Fatalf("expected user message 'Hello', got %v", user)
	}
	if newReq.URL.Path != "/v1/messages" {
		t.Fatalf("expected path /v1/messages, got %s", newReq.URL.Path)
	}
}

func TestResponses2Anthropic_TransformRequest_ToolCall(t *testing.T) {
	p := &Responses2Anthropic{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"input": [
			{"role": "user", "content": "Weather?"},
			{"type": "function_call", "call_id": "call_123", "name": "get_weather", "arguments": "{\"city\":\"London\"}"},
			{"type": "function_call_output", "call_id": "call_123", "output": "Sunny"}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	messages := result["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	// Assistant with tool_use
	assistant := messages[1].(map[string]interface{})
	if assistant["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %v", assistant["role"])
	}
	content := assistant["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_use" {
		t.Fatalf("expected tool_use block, got %v", block["type"])
	}
}

func TestResponses2Anthropic_TransformRequest_Tools(t *testing.T) {
	p := &Responses2Anthropic{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"input": [{"role": "user", "content": "Hello"}],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	tools := result["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "get_weather" {
		t.Fatalf("expected tool name 'get_weather', got %v", tool["name"])
	}
	if tool["input_schema"] == nil {
		t.Fatal("expected input_schema on anthropic tool")
	}
}

func TestResponses2Anthropic_TransformRequest_Reasoning(t *testing.T) {
	p := &Responses2Anthropic{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"input": [{"role": "user", "content": "Think"}],
		"reasoning": {"effort": "high"},
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/responses", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	thinking := result["thinking"].(map[string]interface{})
	if thinking["type"] != "enabled" {
		t.Fatalf("expected thinking type 'enabled', got %v", thinking["type"])
	}
	budget := int(thinking["budget_tokens"].(float64))
	if budget != 16000 {
		t.Fatalf("expected budget 16000 for 'high' effort, got %d", budget)
	}
}

func TestResponses2Anthropic_TransformResponse_NonStreaming(t *testing.T) {
	p := &Responses2Anthropic{}
	respBody := []byte(`{
		"id": "msg_123",
		"model": "claude-sonnet-4-20250514",
		"content": [{"type": "text", "text": "Hello there"}],
		"usage": {"input_tokens": 10, "output_tokens": 20},
		"stop_reason": "end_turn"
	}`)
	httpResp := &http.Response{Header: http.Header{}}
	transformed, err := p.TransformResponse(httpResp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatal(err)
	}
	var result responsesResponse
	if err := json.Unmarshal(transformed, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result.Object != "response" {
		t.Fatalf("expected object 'response', got %s", result.Object)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(result.Output))
	}
	if result.Output[0].Type != "output_text" {
		t.Fatalf("expected output_text type, got %s", result.Output[0].Type)
	}
	if result.Output[0].Text != "Hello there" {
		t.Fatalf("expected text 'Hello there', got %s", result.Output[0].Text)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 20 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
}

func TestResponses2Anthropic_TransformStreamChunk_MessageStart(t *testing.T) {
	p := &Responses2Anthropic{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}
	chunk := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"msg_123","model":"claude-sonnet-4-20250514"}}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "response.created") {
		t.Fatal("expected response.created event, got:", output)
	}
	if !strings.Contains(output, "response.in_progress") {
		t.Fatal("expected response.in_progress event, got:", output)
	}
}

func TestResponses2Anthropic_TransformStreamChunk_TextDelta(t *testing.T) {
	p := &Responses2Anthropic{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}
	// Prime state with message_start
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"msg_123","model":"claude-sonnet-4-20250514"}}`),
	}, ctx)

	// text_delta
	deltaChunk := engine.SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
	}
	result, err := p.TransformStreamChunk(deltaChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "response.output_text.delta") {
		t.Fatal("expected response.output_text.delta event, got:", output)
	}
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected text 'Hello', got:", output)
	}
}

func TestResponses2Anthropic_TransformStreamChunk_MessageStop(t *testing.T) {
	p := &Responses2Anthropic{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}
	// Prime state
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"msg_123","model":"claude-sonnet-4-20250514"}}`),
	}, ctx)
	// Send a text delta first
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
	}, ctx)

	// message_stop
	stopChunk := engine.SSEChunk{
		EventType: "message_stop",
		Data:      []byte(`{"type":"message_stop"}`),
	}
	result, err := p.TransformStreamChunk(stopChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "response.output_text.done") {
		t.Fatal("expected response.output_text.done event, got:", output)
	}
	if !strings.Contains(output, "response.completed") {
		t.Fatal("expected response.completed event, got:", output)
	}
}


// ----- OpenAI2Responses tests -----

func TestOpenAI2Responses_TransformRequest_Basic(t *testing.T) {
	p := &OpenAI2Responses{}
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "Be helpful"},
			{"role": "user", "content": "Hello"}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if result["instructions"] != "Be helpful" {
		t.Fatalf("expected instructions 'Be helpful', got %v", result["instructions"])
	}
	input := result["input"].([]interface{})
	if len(input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(input))
	}
	user := input[0].(map[string]interface{})
	if user["role"] != "user" || user["content"] != "Hello" {
		t.Fatalf("expected user message 'Hello', got %v", user)
	}
	if newReq.URL.Path != "/v1/responses" {
		t.Fatalf("expected path /v1/responses, got %s", newReq.URL.Path)
	}
}

func TestOpenAI2Responses_TransformRequest_ToolCall(t *testing.T) {
	p := &OpenAI2Responses{}
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Weather?"},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "call_123", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"London\"}"}}]},
			{"role": "tool", "tool_call_id": "call_123", "content": "Sunny"}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	input := result["input"].([]interface{})
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}
	fc := input[1].(map[string]interface{})
	if fc["type"] != "function_call" {
		t.Fatalf("expected function_call type, got %v", fc["type"])
	}
	if fc["name"] != "get_weather" {
		t.Fatalf("expected name get_weather, got %v", fc["name"])
	}
	fco := input[2].(map[string]interface{})
	if fco["type"] != "function_call_output" {
		t.Fatalf("expected function_call_output type, got %v", fco["type"])
	}
	if fco["output"] != "Sunny" {
		t.Fatalf("expected output 'Sunny', got %v", fco["output"])
	}
}

func TestOpenAI2Responses_TransformRequest_Reasoning(t *testing.T) {
	p := &OpenAI2Responses{}
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "Think"}],
		"reasoning_effort": "high",
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	reasoning := result["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort 'high', got %v", reasoning["effort"])
	}
}

func TestOpenAI2Responses_TransformResponse_NonStreaming(t *testing.T) {
	p := &OpenAI2Responses{}
	respBody := []byte(`{
		"id": "resp_123",
		"object": "response",
		"status": "completed",
		"model": "gpt-4o",
		"output": [
			{"type": "output_text", "text": "Hello world"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}
	}`)
	httpResp := &http.Response{Header: http.Header{}}
	transformed, err := p.TransformResponse(httpResp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(transformed, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["object"] != "chat.completion" {
		t.Fatalf("expected object 'chat.completion', got %s", result["object"])
	}
	choices := result["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Fatalf("expected content 'Hello world', got %s", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Fatalf("expected role 'assistant', got %s", msg["role"])
	}
}

func TestOpenAI2Responses_TransformStreamChunk_TextDelta(t *testing.T) {
	p := &OpenAI2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"type":"response.created","response":{"id":"resp_123","model":"gpt-4o"}}`),
	}, ctx)

	deltaChunk := engine.SSEChunk{
		EventType: "response.output_text.delta",
		Data:      []byte(`{"type":"response.output_text.delta","output_id":"out_1","delta":"Hello","index":0}`),
	}
	result, err := p.TransformStreamChunk(deltaChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "data: ") {
		t.Fatal("expected 'data: ' prefix, got:", output)
	}
	if !strings.Contains(output, "Hello") {
		t.Fatal("expected content 'Hello', got:", output)
	}
}

func TestOpenAI2Responses_TransformStreamChunk_Completed(t *testing.T) {
	p := &OpenAI2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"type":"response.created","response":{"id":"resp_123","model":"gpt-4o"}}`),
	}, ctx)
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.in_progress",
		Data:      []byte(`{"type":"response.in_progress"}`),
	}, ctx)
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.output_text.delta",
		Data:      []byte(`{"type":"response.output_text.delta","output_id":"out_1","delta":"Hello","index":0}`),
	}, ctx)

	completedChunk := engine.SSEChunk{
		EventType: "response.completed",
		Data:      []byte(`{"type":"response.completed","response":{"id":"resp_123","status":"completed","usage":{"input_tokens":10,"output_tokens":20}}}`),
	}
	result, err := p.TransformStreamChunk(completedChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "finish_reason") {
		t.Fatal("expected finish_reason in completed chunk, got:", output)
	}
	if !strings.Contains(output, "[DONE]") {
		t.Fatal("expected [DONE] marker, got:", output)
	}
}

// ----- Anthropic2Responses tests -----

func TestAnthropic2Responses_TransformRequest_Basic(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"system": "Be helpful",
		"messages": [{"role": "user", "content": "Hello"}],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if result["instructions"] != "Be helpful" {
		t.Fatalf("expected instructions 'Be helpful', got %v", result["instructions"])
	}
	input := result["input"].([]interface{})
	if len(input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(input))
	}
	user := input[0].(map[string]interface{})
	if user["role"] != "user" || user["content"] != "Hello" {
		t.Fatalf("expected user message 'Hello', got %v", user)
	}
	if newReq.URL.Path != "/v1/responses" {
		t.Fatalf("expected path /v1/responses, got %s", newReq.URL.Path)
	}
}

func TestAnthropic2Responses_TransformRequest_ToolCall(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": "Weather?"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_123", "name": "get_weather", "input": {"city": "London"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_123", "content": "Sunny"}]}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	input := result["input"].([]interface{})
	if len(input) < 3 {
		t.Fatalf("expected at least 3 input items, got %d", len(input))
	}
	fc := input[1].(map[string]interface{})
	if fc["type"] != "function_call" {
		t.Fatalf("expected function_call type, got %v", fc["type"])
	}
	if fc["name"] != "get_weather" {
		t.Fatalf("expected name 'get_weather', got %v", fc["name"])
	}
}

func TestAnthropic2Responses_TransformRequest_Thinking(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [{"role": "user", "content": "Think"}],
		"thinking": {"type": "enabled", "budget_tokens": 16000},
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	reasoning := result["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort 'high', got %v", reasoning["effort"])
	}
}

func TestAnthropic2Responses_TransformResponse_NonStreaming(t *testing.T) {
	p := &Anthropic2Responses{}
	respBody := []byte(`{
		"id": "resp_123",
		"object": "response",
		"status": "completed",
		"model": "claude-sonnet-4-20250514",
		"output": [
			{"type": "output_text", "text": "Hello there"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}
	}`)
	httpResp := &http.Response{Header: http.Header{}}
	transformed, err := p.TransformResponse(httpResp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(transformed, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["type"] != "message" {
		t.Fatalf("expected type 'message', got %s", result["type"])
	}
	content := result["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "text" || block["text"] != "Hello there" {
		t.Fatalf("expected text block 'Hello there', got %v", block)
	}
}

func TestAnthropic2Responses_TransformStreamChunk_MessageStart(t *testing.T) {
	p := &Anthropic2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	chunk := engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"type":"response.created","response":{"id":"resp_123","model":"claude-sonnet-4-20250514"}}`),
	}
	result, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "message_start") {
		t.Fatal("expected message_start event, got:", output)
	}
}

func TestAnthropic2Responses_TransformStreamChunk_TextDelta(t *testing.T) {
	p := &Anthropic2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"type":"response.created","response":{"id":"resp_123","model":"claude-sonnet-4-20250514"}}`),
	}, ctx)

	deltaChunk := engine.SSEChunk{
		EventType: "response.output_text.delta",
		Data:      []byte(`{"type":"response.output_text.delta","output_id":"out_1","delta":"Hello","index":0}`),
	}
	result, err := p.TransformStreamChunk(deltaChunk, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	if !strings.Contains(output, "content_block_delta") && !strings.Contains(output, "content_block_start") {
		t.Fatal("expected content_block_delta or content_block_start event, got:", output)
	}
}
