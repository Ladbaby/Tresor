package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"tresor/internal/engine"
)

func TestFixAnthropicImages_NoChange(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "Hello"}},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	// Body should be semantically equivalent (may differ in key ordering)
	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	if len(result["messages"].([]interface{})) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result["messages"].([]interface{})))
	}
	msg := result["messages"].([]interface{})[0].(map[string]interface{})
	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(content))
	}
	part := content[0].(map[string]interface{})
	if part["type"] != "text" {
		t.Fatalf("expected text part, got %v", part["type"])
	}
}

func TestFixAnthropicImages_ExtractSingleImage(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "tool_result",
						"tool_use_id": "tool_123",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "aW1hZ2UtZGF0YQ==",
								},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	messages := result["messages"].([]interface{})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0].(map[string]interface{})
	content := msg["content"].([]interface{})

	// Should have: placeholder text + image (tool_result removed)
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts (placeholder + image), got %d", len(content))
	}

	// First part should be placeholder text
	placeholder := content[0].(map[string]interface{})
	if placeholder["type"] != "text" {
		t.Fatalf("expected placeholder text, got type %v", placeholder["type"])
	}
	if placeholder["text"].(string) == "" {
		t.Fatal("placeholder text should not be empty")
	}

	// Second part should be the promoted image
	img := content[1].(map[string]interface{})
	if img["type"] != "image" {
		t.Fatalf("expected image part, got type %v", img["type"])
	}
	source := img["source"].(map[string]interface{})
	if source["type"] != "base64" {
		t.Fatal("expected base64 source type")
	}
	if source["media_type"] != "image/png" {
		t.Fatalf("expected media_type image/png, got %v", source["media_type"])
	}
	if source["data"] != "aW1hZ2UtZGF0YQ==" {
		t.Fatalf("expected original base64 data, got %v", source["data"])
	}
}

func TestFixAnthropicImages_MixedContent(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Analyze this image:"},
					map[string]interface{}{
						"type": "tool_result",
						"tool_use_id": "tool_123",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/jpeg",
									"data":       "anNvbi1kYXRh",
								},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	messages := result["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].([]interface{})

	// Should have: original text + promoted image (no placeholder since text exists)
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts (text + image), got %d", len(content))
	}

	textPart := content[0].(map[string]interface{})
	if textPart["type"] != "text" {
		t.Fatal("first part should be original text")
	}
	if textPart["text"] != "Analyze this image:" {
		t.Fatalf("text changed unexpectedly: %v", textPart["text"])
	}

	img := content[1].(map[string]interface{})
	if img["type"] != "image" {
		t.Fatal("second part should be promoted image")
	}
}

func TestFixAnthropicImages_ToolResultNoImage(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_123",
						"content":     "plain text result", // string content, not image array
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	// Body should be unchanged (tool_result with string content is preserved)
	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	messages := result["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(content))
	}
	part := content[0].(map[string]interface{})
	if part["type"] != "tool_result" {
		t.Fatalf("tool_result should be preserved, got type %v", part["type"])
	}
}

func TestFixAnthropicImages_MultipleMessages(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "First message"}},
			},
			map[string]interface{}{
				"role":    "assistant",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "OK"}},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Here's another screenshot:"},
					map[string]interface{}{
						"type": "tool_result",
						"tool_use_id": "tool_456",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "c2NyZWVuc2hvdA==",
								},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	messages := result["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// First two messages should be unchanged
	firstMsg := messages[0].(map[string]interface{})
	firstContent := firstMsg["content"].([]interface{})
	if len(firstContent) != 1 || firstContent[0].(map[string]interface{})["type"] != "text" {
		t.Fatal("first message should be unchanged")
	}

	// Third message should have text + promoted image
	thirdMsg := messages[2].(map[string]interface{})
	thirdContent := thirdMsg["content"].([]interface{})
	if len(thirdContent) != 2 {
		t.Fatalf("expected 2 content parts in third message, got %d", len(thirdContent))
	}
	if thirdContent[0].(map[string]interface{})["type"] != "text" {
		t.Fatal("first part of third msg should be text")
	}
	if thirdContent[1].(map[string]interface{})["type"] != "image" {
		t.Fatal("second part of third msg should be promoted image")
	}
}

func TestFixAnthropicImages_ResponseNoOp(t *testing.T) {
	p := &FixAnthropicImages{}
	body := []byte(`{"content":[{"type":"text","text":"response"}]}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}
	if string(newBody) != string(body) {
		t.Fatal("response body should be unchanged")
	}
}

func TestFixAnthropicImages_EmptyBase64Data(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "tool_result",
						"tool_use_id": "tool_123",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "", // empty data — should be skipped
								},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", bytes.NewReader(body))
	ctx := &engine.PipelineContext{}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	// Empty data images should not be extracted — tool_result preserved
	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	messages := result["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content part (tool_result preserved), got %d", len(content))
	}
	part := content[0].(map[string]interface{})
	if part["type"] != "tool_result" {
		t.Fatalf("tool_result should be preserved for empty data, got type %v", part["type"])
	}
}
