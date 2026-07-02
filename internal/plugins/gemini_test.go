package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// ---------------- OpenAI2Gemini ----------------

func TestOpenAI2Gemini_TransformRequest_Basic(t *testing.T) {
	p := &OpenAI2Gemini{}

	body, _ := json.Marshal(map[string]interface{}{
		"model": "gemini-2.5-pro",
		"messages": []map[string]interface{}{
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello"},
		},
		"max_tokens":  256,
		"temperature": 0.4,
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1/chat/completions", bytes.NewReader(body))
	ctx := &engine.PipelineContext{
		TargetDownstream: &engine.Downstream{
			APIKey:     "sk-gemini",
			ApiFormats: []string{"gemini"},
		},
	}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(newReq.URL.Path, "/v1beta/models/gemini-2.5-pro:generateContent") {
		t.Fatalf("expected path /v1beta/models/gemini-2.5-pro:generateContent, got %s", newReq.URL.Path)
	}
	if newReq.Header.Get("x-goog-api-key") != "sk-gemini" {
		t.Fatalf("expected x-goog-api-key sk-gemini, got %q", newReq.Header.Get("x-goog-api-key"))
	}

	var geminiReq map[string]interface{}
	if err := json.Unmarshal(newBody, &geminiReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	sys, _ := geminiReq["systemInstruction"].(map[string]interface{})
	if sys == nil {
		t.Fatalf("expected systemInstruction, got %v", geminiReq["systemInstruction"])
	}
	sysPartsRaw, _ := sys["parts"].([]interface{})
	if len(sysPartsRaw) != 1 {
		t.Fatalf("expected 1 system part, got %d", len(sysPartsRaw))
	}
	sysPart0, _ := sysPartsRaw[0].(map[string]interface{})
	if sysPart0["text"] != "You are helpful." {
		t.Fatalf("expected system text 'You are helpful.', got %v", sysPart0["text"])
	}

	contents, _ := geminiReq["contents"].([]interface{})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	c0, _ := contents[0].(map[string]interface{})
	if c0["role"] != "user" {
		t.Fatalf("expected user role, got %v", c0["role"])
	}

	genCfg, _ := geminiReq["generationConfig"].(map[string]interface{})
	if genCfg == nil {
		t.Fatalf("expected generationConfig, got %v", geminiReq["generationConfig"])
	}
	if genCfg["maxOutputTokens"] != float64(256) {
		t.Fatalf("expected maxOutputTokens 256, got %v", genCfg["maxOutputTokens"])
	}
	if genCfg["temperature"] != float64(0.4) {
		t.Fatalf("expected temperature 0.4, got %v", genCfg["temperature"])
	}
}

func TestOpenAI2Gemini_TransformRequest_Tools(t *testing.T) {
	p := &OpenAI2Gemini{}

	body, _ := json.Marshal(map[string]interface{}{
		"model": "gemini-2.5-pro",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "What is the weather?"},
		},
		"tools": []map[string]interface{}{
			{"type": "function", "function": map[string]interface{}{
				"name":        "get_weather",
				"description": "Get the weather",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}}},
			}},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "k"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var geminiReq map[string]interface{}
	_ = json.Unmarshal(newBody, &geminiReq)

	tools, _ := geminiReq["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	t0, _ := tools[0].(map[string]interface{})
	fd, _ := t0["functionDeclarations"].([]interface{})
	if len(fd) != 1 {
		t.Fatalf("expected 1 functionDeclaration, got %d", len(fd))
	}
	fd0, _ := fd[0].(map[string]interface{})
	if fd0["name"] != "get_weather" {
		t.Fatalf("expected name get_weather, got %v", fd0["name"])
	}
}

func TestOpenAI2Gemini_TransformRequest_Streaming(t *testing.T) {
	p := &OpenAI2Gemini{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gemini-2.5-pro",
		"messages": []map[string]interface{}{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "k"}}

	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(newReq.URL.Path, ":streamGenerateContent") {
		t.Fatalf("expected streamGenerateContent in path, got %s", newReq.URL.Path)
	}
	if !strings.Contains(newReq.URL.RawQuery, "alt=sse") {
		t.Fatalf("expected alt=sse query, got %s", newReq.URL.RawQuery)
	}
}

func TestOpenAI2Gemini_TransformResponse_JSON(t *testing.T) {
	p := &OpenAI2Gemini{}

	geminiResp := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "Hello there"}]}, "finishReason": "STOP", "index": 0}],
		"usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 2, "totalTokenCount": 7},
		"modelVersion": "gemini-2.5-pro"
	}`)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(resp, geminiResp, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var oai map[string]interface{}
	if err := json.Unmarshal(out, &oai); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	choices, _ := oai["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "Hello there" {
		t.Fatalf("expected content 'Hello there', got %v", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("expected finish_reason 'stop', got %v", choice["finish_reason"])
	}
	usage := oai["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != float64(5) {
		t.Fatalf("expected prompt_tokens 5, got %v", usage["prompt_tokens"])
	}
}

func TestOpenAI2Gemini_TransformResponse_ToolCall(t *testing.T) {
	p := &OpenAI2Gemini{}

	geminiResp := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}
			]},
			"finishReason": "STOP",
			"index": 0
		}]
	}`)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(resp, geminiResp, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var oai map[string]interface{}
	_ = json.Unmarshal(out, &oai)
	choices := oai["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	tcs, _ := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("expected name get_weather, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "Paris") {
		t.Fatalf("expected arguments to contain Paris, got %s", argsStr)
	}
}

// ---------------- Anthropic2Gemini ----------------

func TestAnthropic2Gemini_TransformRequest_Basic(t *testing.T) {
	p := &Anthropic2Gemini{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "gemini-2.5-pro",
		"max_tokens": 256,
		"system":     "You are helpful.",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "k"}}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(newReq.URL.Path, "/v1beta/models/gemini-2.5-pro:generateContent") {
		t.Fatalf("expected Gemini URL, got %s", newReq.URL.Path)
	}

	var geminiReq map[string]interface{}
	_ = json.Unmarshal(newBody, &geminiReq)

	sys, _ := geminiReq["systemInstruction"].(map[string]interface{})
	if sys == nil {
		t.Fatal("expected systemInstruction")
	}
	contents, _ := geminiReq["contents"].([]interface{})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	if contents[0].(map[string]interface{})["role"] != "user" {
		t.Fatalf("expected user role, got %v", contents[0])
	}
	genCfg, _ := geminiReq["generationConfig"].(map[string]interface{})
	if genCfg == nil || genCfg["maxOutputTokens"] != float64(256) {
		t.Fatalf("expected maxOutputTokens 256, got %v", genCfg)
	}
}

func TestAnthropic2Gemini_TransformRequest_ToolResult(t *testing.T) {
	p := &Anthropic2Gemini{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "gemini-2.5-pro",
		"max_tokens": 256,
		"messages": []map[string]interface{}{
			{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": "get_weather", "content": "72°F"},
			}},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "k"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var geminiReq map[string]interface{}
	_ = json.Unmarshal(newBody, &geminiReq)
	contents, _ := geminiReq["contents"].([]interface{})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	partsRaw, _ := contents[0].(map[string]interface{})["parts"].([]interface{})
	if len(partsRaw) != 1 {
		t.Fatalf("expected 1 part, got %d", len(partsRaw))
	}
	fr, ok := partsRaw[0].(map[string]interface{})["functionResponse"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected functionResponse, got %v", partsRaw[0])
	}
	if fr["name"] != "get_weather" {
		t.Fatalf("expected name get_weather, got %v", fr["name"])
	}
}

func TestAnthropic2Gemini_TransformResponse_JSON(t *testing.T) {
	p := &Anthropic2Gemini{}

	geminiResp := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "Hi"}]}, "finishReason": "STOP", "index": 0}],
		"modelVersion": "gemini-2.5-pro"
	}`)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(resp, geminiResp, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var anth map[string]interface{}
	_ = json.Unmarshal(out, &anth)
	if anth["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %v", anth["role"])
	}
	content, _ := anth["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "text" || block["text"] != "Hi" {
		t.Fatalf("unexpected content block: %v", block)
	}
	if anth["stop_reason"] != "end_turn" {
		t.Fatalf("expected end_turn, got %v", anth["stop_reason"])
	}
}

// ---------------- Gemini2OpenAI ----------------

func TestGemini2OpenAI_TransformRequest_Basic(t *testing.T) {
	p := &Gemini2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Hi"}}},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{{"text": "Be concise."}},
		},
		"generationConfig": map[string]interface{}{
			"maxOutputTokens": 128,
			"temperature":     0.3,
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gemini-2.5-pro:generateContent", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-openai"}}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if newReq.URL.Path != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions, got %s", newReq.URL.Path)
	}
	if !strings.HasPrefix(newReq.Header.Get("Authorization"), "Bearer ") {
		t.Fatalf("expected Bearer auth, got %q", newReq.Header.Get("Authorization"))
	}

	var oaiReq map[string]interface{}
	_ = json.Unmarshal(newBody, &oaiReq)

	if oaiReq["model"] != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %v", oaiReq["model"])
	}
	if oaiReq["max_tokens"] != float64(128) {
		t.Fatalf("expected max_tokens 128, got %v", oaiReq["max_tokens"])
	}
	msgs, _ := oaiReq["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	m0 := msgs[0].(map[string]interface{})
	m1 := msgs[1].(map[string]interface{})
	if m0["role"] != "system" || m0["content"] != "Be concise." {
		t.Fatalf("expected system message, got %v", m0)
	}
	if m1["role"] != "user" || m1["content"] != "Hi" {
		t.Fatalf("expected user message, got %v", m1)
	}
}

func TestGemini2OpenAI_TransformResponse_JSON(t *testing.T) {
	p := &Gemini2OpenAI{}

	oaiResp := []byte(`{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello there"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
	}`)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(resp, oaiResp, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var gem map[string]interface{}
	_ = json.Unmarshal(out, &gem)
	candidates, _ := gem["candidates"].([]interface{})
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0].(map[string]interface{})
	content := c["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	if len(parts) != 1 || parts[0].(map[string]interface{})["text"] != "Hello there" {
		t.Fatalf("expected text 'Hello there', got %v", parts)
	}
	if c["finishReason"] != "STOP" {
		t.Fatalf("expected STOP, got %v", c["finishReason"])
	}
}

// ---------------- Gemini2Anthropic ----------------

func TestGemini2Anthropic_TransformRequest_Basic(t *testing.T) {
	p := &Gemini2Anthropic{}

	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Hello"}}},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{{"text": "Helpful."}},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gemini-2.5-pro:generateContent", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-ant"}}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if newReq.URL.Path != "/v1/messages" {
		t.Fatalf("expected /v1/messages, got %s", newReq.URL.Path)
	}
	if newReq.Header.Get("x-api-key") != "sk-ant" {
		t.Fatalf("expected x-api-key sk-ant, got %q", newReq.Header.Get("x-api-key"))
	}
	if newReq.Header.Get("anthropic-version") == "" {
		t.Fatalf("expected anthropic-version header")
	}

	var anth map[string]interface{}
	_ = json.Unmarshal(newBody, &anth)
	if anth["system"] != "Helpful." {
		t.Fatalf("expected system 'Helpful.', got %v", anth["system"])
	}
	if anth["model"] != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %v", anth["model"])
	}
	msgs, _ := anth["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

// TestGemini2Anthropic_TransformRequest_Streaming is a regression test for the
// bug where a Gemini :streamGenerateContent request routed to an Anthropic
// downstream did NOT propagate stream=true. As a result the upstream returned
// a single JSON body while the client still expected SSE, so the response
// appeared empty.
func TestGemini2Anthropic_TransformRequest_Streaming(t *testing.T) {
	p := &Gemini2Anthropic{}

	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Hi"}}},
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-ant"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var anth map[string]interface{}
	_ = json.Unmarshal(newBody, &anth)
	if anth["stream"] != true {
		t.Fatalf("expected stream=true when input path is :streamGenerateContent, got %v", anth["stream"])
	}
}

// ---------------- geminiModelFromPath ----------------

func TestGeminiModelFromPathLocal(t *testing.T) {
	tests := []struct {
		path, want string
	}{
		{"/v1beta/models/gemini-2.5-pro", "gemini-2.5-pro"},
		{"/v1beta/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{"/v1beta/models/gemini-2.5-pro:streamGenerateContent", "gemini-2.5-pro"},
		{"/v1beta/models", ""},
		{"/v1beta/models/", ""},
		{"/v1/chat/completions", ""},
	}
	for _, tt := range tests {
		if got := geminiModelFromPath(tt.path); got != tt.want {
			t.Errorf("geminiModelFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// ---------------- Registry ----------------

func TestRegistry_HasGeminiPlugins(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"openai2gemini", "anthropic2gemini", "gemini2openai", "gemini2anthropic"} {
		if _, err := r.CreatePlugin(id, nil); err != nil {
			t.Errorf("expected plugin %s to be registered, got error: %v", id, err)
		}
	}
}