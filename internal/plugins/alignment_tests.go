package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// ----- Alignment with llama.cpp reference tests -----

func TestNormalizeAnthropicBillingHeader(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "billing header present",
			input:    "x-anthropic-billing-header:cch=abcde;other=val",
			expected: "x-anthropic-billing-header:cch=fffff;other=val",
		},
		{
			name:     "no prefix",
			input:    "Be concise.",
			expected: "Be concise.",
		},
		{
			name:     "prefix but no cch=",
			input:    "x-anthropic-billing-header:other=val",
			expected: "x-anthropic-billing-header:other=val",
		},
		{
			name:     "cch= but wrong length after",
			input:    "x-anthropic-billing-header:cch=abc;other=val",
			expected: "x-anthropic-billing-header:cch=abc;other=val",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeAnthropicBillingHeader(tt.input)
			if got != tt.expected {
				t.Fatalf("normalizeAnthropicBillingHeader(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestAnthropic2OpenAI_TransformRequest_ThinkingParam(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"thinking":   map[string]interface{}{"type": "enabled", "budget_tokens": 8000},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	bt, ok := openAIReq["thinking_budget_tokens"]
	if !ok {
		t.Fatal("expected thinking_budget_tokens in OpenAI body")
	}
	if bt != float64(8000) {
		t.Fatalf("expected thinking_budget_tokens 8000, got %v", bt)
	}
}

func TestAnthropic2OpenAI_TransformRequest_ThinkingParam_DefaultBudget(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// When type is "enabled" but no budget_tokens, should default to 10000
	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"thinking":   map[string]interface{}{"type": "enabled"},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	bt, ok := openAIReq["thinking_budget_tokens"]
	if !ok {
		t.Fatal("expected thinking_budget_tokens in OpenAI body")
	}
	if bt != float64(10000) {
		t.Fatalf("expected thinking_budget_tokens 10000 (default), got %v", bt)
	}
}

func TestAnthropic2OpenAI_TransformRequest_Metadata(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"metadata":   map[string]interface{}{"user_id": "user123"},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	uid, ok := openAIReq["__metadata_user_id"]
	if !ok {
		t.Fatal("expected __metadata_user_id in OpenAI body")
	}
	if uid != "user123" {
		t.Fatalf("expected __metadata_user_id 'user123', got %v", uid)
	}
}

func TestAnthropic2OpenAI_TransformRequest_TopPandTopK(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"top_p":      0.9,
		"top_k":      40,
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	topP, ok := openAIReq["top_p"]
	if !ok {
		t.Fatal("expected top_p in OpenAI body")
	}
	if topP != float64(0.9) {
		t.Fatalf("expected top_p 0.9, got %v", topP)
	}

	topK, ok := openAIReq["top_k"]
	if !ok {
		t.Fatal("expected top_k in OpenAI body")
	}
	if topK != float64(40) {
		t.Fatalf("expected top_k 40, got %v", topK)
	}
}

func TestAnthropic2OpenAI_TransformRequest_MaxTokensDefault(t *testing.T) {
	p := &Anthropic2OpenAI{}

	// No max_tokens field — should default to 4096
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "claude-sonnet-4-20250514",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})

	req, _ := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(newBody, &openAIReq)

	mt, ok := openAIReq["max_tokens"]
	if !ok {
		t.Fatal("expected max_tokens in OpenAI body")
	}
	if mt != float64(4096) {
		t.Fatalf("expected max_tokens 4096 (default), got %v", mt)
	}
}

func TestAnthropic2OpenAI_TransformRequest_ThinkingAndToolUse(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "What's the weather?"},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "text": "Let me think about this..."},
					map[string]interface{}{"type": "text", "text": "I'll look it up."},
					map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": map[string]interface{}{"location": "Paris"}},
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

	assistantMsg := messages[1].(map[string]interface{})

	content, _ := assistantMsg["content"].(string)
	if content != "I'll look it up." {
		t.Fatalf("expected content 'I'll look it up.', got %v", content)
	}

	reasoning, hasReasoning := assistantMsg["reasoning_content"]
	if !hasReasoning {
		t.Fatal("expected reasoning_content field")
	}
	if reasoning != "Let me think about this..." {
		t.Fatalf("expected reasoning_content 'Let me think about this...', got %v", reasoning)
	}

	tcs, hasTC := assistantMsg["tool_calls"]
	if !hasTC {
		t.Fatal("expected tool_calls field")
	}
	tcArr := tcs.([]interface{})
	if len(tcArr) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcArr))
	}
}

func TestAnthropic2OpenAI_TransformRequest_BillingHeaderSystemPrompt(t *testing.T) {
	p := &Anthropic2OpenAI{}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 100,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"system":     "x-anthropic-billing-header:cch=abcde;other=val",
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
	if len(messages) == 0 {
		t.Fatal("expected at least 1 message")
	}
	sysMsg := messages[0].(map[string]interface{})
	content := sysMsg["content"].(string)

	if strings.Contains(content, "cch=abcde") {
		t.Fatal("billing header cch value should have been scrubbed")
	}
	if !strings.Contains(content, "cch=fffff") {
		t.Fatal("billing header cch value should be replaced with fffff")
	}
}
