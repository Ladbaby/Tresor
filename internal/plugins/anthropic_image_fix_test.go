package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

func TestFixAnthropicImages_ToolResultMixedTextAndImage(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Look at this:"},
					map[string]interface{}{
						"type": "tool_result",
						"tool_use_id": "tool_123",
						"content": []interface{}{
							map[string]interface{}{
								"type": "text",
								"text": "The screenshot shows the login page",
							},
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
	json.Unmarshal(newBody, &result)

	messages := result["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].([]interface{})

	// Should have: original text + preserved tool_result text + promoted image
	if len(content) != 3 {
		t.Fatalf("expected 3 content parts (text + preserved text + image), got %d", len(content))
	}

	// First part: original text
	if content[0].(map[string]interface{})["type"] != "text" {
		t.Fatal("first part should be original text")
	}
	if content[0].(map[string]interface{})["text"] != "Look at this:" {
		t.Fatal("first part text changed")
	}

	// Second part: preserved tool_result text
	if content[1].(map[string]interface{})["type"] != "text" {
		t.Fatal("second part should be preserved text from tool_result")
	}
	if content[1].(map[string]interface{})["text"] != "The screenshot shows the login page" {
		t.Fatalf("preserved text incorrect: %v", content[1].(map[string]interface{})["text"])
	}

	// Third part: promoted image
	if content[2].(map[string]interface{})["type"] != "image" {
		t.Fatal("third part should be promoted image")
	}
}

func TestFixAnthropicImages_ResponseToolUseInThinkingArray(t *testing.T) {
	p := &FixAnthropicImages{}

	// Simulates a backend response where thinking.content is an array
	// containing both text and tool_use blocks
	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{
				"type": "thinking",
				"content": [
					{"type": "text", "text": "Let me analyze the code..."},
					{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": {"path": "main.go"}}
				]
			},
			{"type": "text", "text": "Here is my analysis:"}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	content := result["content"].([]interface{})
	// Expected: thinking (cleaned, only text), tool_use (promoted), text (original)
	if len(content) != 3 {
		t.Fatalf("expected 3 content parts, got %d", len(content))
	}

	// Part 0: cleaned thinking (only text, no tool_use)
	thinking := content[0].(map[string]interface{})
	if thinking["type"] != "thinking" {
		t.Fatalf("expected thinking, got %v", thinking["type"])
	}
	thinkingContent := thinking["content"].([]interface{})
	if len(thinkingContent) != 1 {
		t.Fatalf("thinking should have 1 part (text), got %d", len(thinkingContent))
	}
	if thinkingContent[0].(map[string]interface{})["type"] != "text" {
		t.Fatal("thinking content should be text")
	}

	// Part 1: promoted tool_use
	toolUse := content[1].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Fatalf("expected promoted tool_use, got %v", toolUse["type"])
	}
	if toolUse["name"] != "read_file" {
		t.Fatalf("tool_use name incorrect: %v", toolUse["name"])
	}

	// Part 2: original text part
	textPart := content[2].(map[string]interface{})
	if textPart["type"] != "text" {
		t.Fatal("third part should be original text")
	}
}

func TestFixAnthropicImages_ResponseToolUseInThinkingText(t *testing.T) {
	p := &FixAnthropicImages{}

	// Simulates a backend response where thinking.text contains embedded
	// JSON tool_use blocks mixed with natural language text
	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{
				"type": "thinking",
				"text": "Let me check the code first.\n{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"read_file\",\"input\":{\"path\":\"main.go\"}}\nNow I have the code."
			},
			{"type": "text", "text": "Analysis complete."}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	content := result["content"].([]interface{})
	// Expected: thinking (cleaned text), tool_use (promoted), text (original)
	if len(content) != 3 {
		t.Fatalf("expected 3 content parts, got %d: %s", len(content), string(newBody))
	}

	// Part 0: cleaned thinking text (JSON replaced with reference)
	thinking := content[0].(map[string]interface{})
	if thinking["type"] != "thinking" {
		t.Fatalf("expected thinking, got %v", thinking["type"])
	}
	thinkingText := thinking["text"].(string)
	if !strings.Contains(thinkingText, "Let me check the code first") {
		t.Fatal("thinking text should preserve surrounding text")
	}
	if !strings.Contains(thinkingText, "[tool_use:") {
		t.Fatal("thinking text should contain tool_use reference")
	}

	// Part 1: promoted tool_use
	toolUse := content[1].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Fatalf("expected promoted tool_use, got %v", toolUse["type"])
	}
	if toolUse["name"] != "read_file" {
		t.Fatalf("tool_use name incorrect: %v", toolUse["name"])
	}
}

func TestFixAnthropicImages_ResponseNoToolUseInThinking(t *testing.T) {
	p := &FixAnthropicImages{}

	// Normal thinking response — should pass through unchanged
	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "thinking", "text": "Let me think about this..."},
			{"type": "text", "text": "Here is my answer."}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal(newBody, &result)

	content := result["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts (unchanged), got %d", len(content))
	}
	if content[0].(map[string]interface{})["type"] != "thinking" {
		t.Fatal("first part should be thinking")
	}
	if content[1].(map[string]interface{})["type"] != "text" {
		t.Fatal("second part should be text")
	}
}

func TestFixAnthropicImages_StreamToolUseInThinking(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	// Simulate SSE stream: thinking block starts, tool_use block emitted inside,
	// then thinking continues
	events := []engine.SSEChunk{
		// Enter thinking block
		{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"thinking"}`)},
		// Thinking delta
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"text_delta","text":"Let me"}}`)},
		// Nested tool_use block starts inside thinking
		{EventType: "content_block_start", Data: []byte(`{"index":1,"type":"tool_use","id":"tu_1","name":"read_file"}`)},
		// Tool_use delta
		{EventType: "content_block_delta", Data: []byte(`{"index":1,"delta":{"input_json":"{\"path\""}}`)},
		// Tool_use block completes
		{EventType: "content_block_stop", Data: []byte(`{"index":1}`)},
		// Thinking continues
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"text_delta","text":" done."}}`)},
		// Thinking block ends
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
	}

	var results []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		results = append(results, out)
	}

	// Verify:
	// 1. thinking content_block_start passes through
	// 2. First thinking delta passes through
	// 3. Nested tool_use events are absorbed (empty data)
	// 4. When tool_use completes, buffered events are emitted as raw SSE
	// 5. Remaining thinking delta passes through (insideThinking restored)
	// 6. Thinking block stop passes through

	if len(results) != 7 {
		t.Fatalf("expected 7 result chunks, got %d", len(results))
	}

	// Chunk 0: thinking content_block_start (passed through)
	if results[0].EventType != "content_block_start" {
		t.Errorf("chunk 0: expected content_block_start, got %s", results[0].EventType)
	}

	// Chunk 1: thinking delta (passed through)
	if results[1].EventType != "content_block_delta" {
		t.Errorf("chunk 1: expected content_block_delta, got %s", results[1].EventType)
	}

	// Chunk 2: tool_use content_block_start (absorbed — empty data)
	if len(results[2].Data) != 0 {
		t.Error("chunk 2: tool_use start should be absorbed (empty data)")
	}

	// Chunk 3: tool_use delta (absorbed — empty data)
	if len(results[3].Data) != 0 {
		t.Error("chunk 3: tool_use delta should be absorbed (empty data)")
	}

	// Chunk 4: tool_use content_block_stop triggers flush — emits raw SSE
	if !strings.Contains(string(results[4].Data), "\n\n") {
		t.Error("chunk 4: should contain raw SSE with event boundaries")
	}
	if !strings.Contains(string(results[4].Data), "tool_use") {
		t.Error("chunk 4: should contain tool_use event data")
	}

	// Chunk 5: thinking delta (passed through after insideThinking restored)
	if results[5].EventType != "content_block_delta" {
		t.Errorf("chunk 5: expected content_block_delta, got %s", results[5].EventType)
	}

	// Chunk 6: thinking block stop (passed through)
	if results[6].EventType != "content_block_stop" {
		t.Errorf("chunk 6: expected content_block_stop, got %s", results[6].EventType)
	}
}

func TestFixAnthropicImages_StreamNoChange(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	// Normal stream without thinking — should pass through unchanged
	events := []engine.SSEChunk{
		{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"text"}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
	}

	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform: %v", err)
		}
		if !bytes.Equal(out.Data, ev.Data) {
			t.Errorf("data changed: %s -> %s", string(ev.Data), string(out.Data))
		}
		if out.EventType != ev.EventType {
			t.Errorf("event type changed: %s -> %s", ev.EventType, out.EventType)
		}
	}
}

// --- Regression tests for review findings ---

// F1: Two tool_use blocks in one thinking string must not corrupt the
// cleaned text. Previously the code spliced `cleaned` in place using
// stale indices, which panicked on the second object.
func TestFixAnthropicImages_ResponseMultipleToolUseInThinkingText(t *testing.T) {
	p := &FixAnthropicImages{}

	body := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{
				"type": "thinking",
				"text": "Plan: {\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"read_file\",\"input\":{\"path\":\"a.go\"}} then {\"type\":\"tool_use\",\"id\":\"tu_2\",\"name\":\"write_file\",\"input\":{\"path\":\"b.go\"}} done"
			},
			{"type": "text", "text": "OK"}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	content := result["content"].([]interface{})
	if len(content) != 4 {
		t.Fatalf("expected 4 parts (thinking + 2 tool_use + text), got %d: %s", len(content), string(newBody))
	}

	thinking := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(thinking, "Plan:") || !strings.Contains(thinking, "done") {
		t.Fatalf("cleaned thinking should preserve surrounding prose, got: %q", thinking)
	}
	if !strings.Contains(thinking, "[tool_use:") {
		t.Fatal("cleaned thinking should contain tool_use references")
	}
	// Two distinct placeholders, no double-substitution.
	if strings.Count(thinking, "[tool_use:") != 2 {
		t.Fatalf("expected 2 tool_use placeholders, got %d in %q", strings.Count(thinking, "[tool_use:"), thinking)
	}

	if content[1].(map[string]interface{})["name"] != "read_file" {
		t.Fatal("first promoted tool_use should be read_file")
	}
	if content[2].(map[string]interface{})["name"] != "write_file" {
		t.Fatal("second promoted tool_use should be write_file")
	}
	if content[3].(map[string]interface{})["type"] != "text" {
		t.Fatal("last part should be original text")
	}
}

// F2: A content_block_stop with an index that does NOT match the buffered
// tool_use's index (e.g. the outer thinking block's stop, or a text block's
// stop, or an earlier tool_use's stop) must pass through immediately.
// Buffering these stops previously produced a malformed stream where the
// buffer was flushed late, rearranging stop events with no matching start —
// the Anthropic SDK rejected it with "Content block not found".
func TestFixAnthropicImages_StreamToolUseStopIndexMismatch(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	events := []engine.SSEChunk{
		{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"thinking"}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"text_delta","text":"Let me"}}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":1,"type":"tool_use","id":"tu_1","name":"read_file"}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":1,"delta":{"input_json":"{\"path\""}}`)},
		// Outer thinking stop arrives FIRST (out-of-order) — must pass through.
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
		// Then the actual tool_use stop — triggers flush.
		{EventType: "content_block_stop", Data: []byte(`{"index":1}`)},
		// Then the thinking stop again (already past) — must pass through.
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
	}

	var results []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		results = append(results, out)
	}

	// Result 4 is the index=0 stop — must pass through unchanged.
	if results[4].EventType != "content_block_stop" {
		t.Errorf("chunk 4 (thinking stop, index mismatch): expected pass-through, got event=%q", results[4].EventType)
	}
	if !bytes.Equal(results[4].Data, events[4].Data) {
		t.Errorf("chunk 4 (thinking stop): expected data=%q, got %q", string(events[4].Data), string(results[4].Data))
	}
	// Result 5 is the index=1 stop — must trigger the flush with all buffered
	// events for the tool_use.
	if !strings.Contains(string(results[5].Data), "\n\n") {
		t.Error("chunk 5 (matching tool_use stop): expected raw SSE flush")
	}
	if !strings.Contains(string(results[5].Data), "tu_1") {
		t.Error("chunk 5: flush should contain tool_use id")
	}
	// Result 6 is a stray index=0 stop — must pass through (not buffered again).
	if results[6].EventType != "content_block_stop" {
		t.Errorf("chunk 6: expected pass-through, got event=%q", results[6].EventType)
	}
	if !bytes.Equal(results[6].Data, events[6].Data) {
		t.Errorf("chunk 6: expected pass-through data, got %q", string(results[6].Data))
	}
}

// F3a: message_stop arriving while trackingToolUse=true must flush first.
func TestFixAnthropicImages_StreamMessageStopFlushes(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	events := []engine.SSEChunk{
		{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"thinking"}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":1,"type":"tool_use","id":"tu_1","name":"x"}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":1,"delta":{"input_json":"{}"}}`)},
		// No content_block_stop for the tool_use — terminated by message_stop.
		{EventType: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}

	var results []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		results = append(results, out)
	}

	// The message_stop chunk must emit the buffered tool_use events.
	last := results[len(results)-1]
	if !strings.Contains(string(last.Data), "tu_1") {
		t.Errorf("message_stop should flush buffered tool_use; got data=%q", string(last.Data))
	}
	if !strings.Contains(string(last.Data), "\n\n") {
		t.Error("message_stop flush should be raw SSE")
	}
	// State must be reset — a follow-up chunk should not think we're still
	// inside a tool_use.
	out, err := p.TransformStreamChunk(engine.SSEChunk{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"text"}`)}, ctx)
	if err != nil {
		t.Fatalf("post-reset transform: %v", err)
	}
	if out.EventType != "content_block_start" {
		t.Errorf("post-reset chunk should pass through; got event=%q", out.EventType)
	}
}

// F3b: synthetic [DONE] (engine.go:701) must also flush buffered tool_use.
func TestFixAnthropicImages_StreamDoneFlushes(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	events := []engine.SSEChunk{
		{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"thinking"}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":1,"type":"tool_use","id":"tu_x","name":"y"}`)},
		// Aborted mid-tool-use — no content_block_stop.
		{EventType: "", Data: []byte("[DONE]")},
	}

	var results []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		results = append(results, out)
	}

	last := results[len(results)-1]
	if !strings.Contains(string(last.Data), "tu_x") {
		t.Errorf("[DONE] should flush buffered tool_use; got data=%q", string(last.Data))
	}
}

// F3c: same scenario as 3b but the stream ends without any termination
// event. Verify the state-reset logic that the new code added doesn't
// itself introduce a leak. (This is a state-shape test, not a wire test.)
func TestFixAnthropicImages_StreamAbortedMidToolUse(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	// Begin a tool_use and feed [DONE] immediately, no deltas, no stop.
	p.TransformStreamChunk(engine.SSEChunk{EventType: "content_block_start", Data: []byte(`{"index":0,"type":"thinking"}`)}, ctx)
	p.TransformStreamChunk(engine.SSEChunk{EventType: "content_block_start", Data: []byte(`{"index":1,"type":"tool_use","id":"tu_abort","name":"z"}`)}, ctx)
	out, err := p.TransformStreamChunk(engine.SSEChunk{EventType: "", Data: []byte("[DONE]")}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(string(out.Data), "tu_abort") {
		t.Fatalf("aborted stream should still flush buffered tool_use; got data=%q", string(out.Data))
	}
}

// F4: tool_use whose input contains `}` inside a string must still parse.
func TestFixAnthropicImages_ResponseToolUseWithBraceInString(t *testing.T) {
	p := &FixAnthropicImages{}

	body := []byte(`{
		"id": "msg_x",
		"type": "message",
		"role": "assistant",
		"content": [
			{
				"type": "thinking",
				"text": "Note: {\"type\":\"tool_use\",\"id\":\"tu_brace\",\"name\":\"write\",\"input\":{\"content\":\"hello } world\"}} done"
			}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	content := result["content"].([]interface{})
	if len(content) < 2 {
		t.Fatalf("expected at least thinking + promoted tool_use, got %d: %s", len(content), string(newBody))
	}
	var foundToolUse bool
	for _, part := range content {
		m := part.(map[string]interface{})
		if m["type"] == "tool_use" && m["name"] == "write" {
			foundToolUse = true
			id, _ := m["id"].(string)
			if id != "tu_brace" {
				t.Fatalf("tool_use id mismatch: %v", id)
			}
		}
	}
	if !foundToolUse {
		t.Fatalf("expected promoted tool_use named 'write', got: %s", string(newBody))
	}
}

// F5: tool_result.content with numbers/booleans/null alongside an image
// must have the primitives preserved as text (rather than silently dropped)
// while the image is promoted to top level.
func TestFixAnthropicImages_ToolResultPreservesNonStringPrimitives(t *testing.T) {
	p := &FixAnthropicImages{}

	payload := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_prim",
						"content": []interface{}{
							float64(42),
							true,
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "AAAA",
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
	_, newBody, err := p.TransformRequest(req, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal(newBody, &result)
	msg := result["messages"].([]interface{})[0].(map[string]interface{})
	content := msg["content"].([]interface{})

	// Find the preserved primitives text — combined form "42\ntrue".
	var primitives string
	var hasImage bool
	for _, p := range content {
		pm := p.(map[string]interface{})
		switch pm["type"] {
		case "text":
			if s, ok := pm["text"].(string); ok && strings.Contains(s, "42") {
				primitives = s
			}
		case "image":
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatal("image should be promoted to top level")
	}
	if primitives != "42\ntrue" {
		t.Fatalf("expected primitives preserved as '42\\ntrue', got %q (content=%v)", primitives, content)
	}
}

// F8: Case 3 — thinking.text as an array of parts containing a tool_use
// must promote the tool_use.
func TestFixAnthropicImages_ResponseCase3ThinkingTextArray(t *testing.T) {
	p := &FixAnthropicImages{}

	body := []byte(`{
		"id": "msg_c3",
		"type": "message",
		"role": "assistant",
		"content": [
			{
				"type": "thinking",
				"text": [
					{"type": "text", "text": "let me read"},
					{"type": "tool_use", "id": "tu_c3", "name": "read_file", "input": {"path": "x"}}
				]
			},
			{"type": "text", "text": "done"}
		]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(newBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	content := result["content"].([]interface{})
	if len(content) != 3 {
		t.Fatalf("expected 3 parts (thinking + tool_use + text), got %d", len(content))
	}
	toolUse := content[1].(map[string]interface{})
	if toolUse["type"] != "tool_use" || toolUse["name"] != "read_file" {
		t.Fatalf("expected promoted tool_use, got %v", toolUse)
	}
	// Thinking part should retain only the text portion of the array.
	thinking := content[0].(map[string]interface{})
	arr := thinking["text"].([]interface{})
	if len(arr) != 1 || arr[0].(map[string]interface{})["text"] != "let me read" {
		t.Fatalf("thinking text array should keep only the text part, got %v", arr)
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

// Regression test for the "Content block not found" bug observed in
// Claude Code when fix_anthropic_images was enabled on a CoT backend that
// emits multiple tool_use blocks inside a single thinking block.
//
// Pre-fix behavior: each new nested tool_use start silently overwrote the
// previously buffered events (line 169 `t.bufferedToolUse = nil` ran while
// trackingToolUse was still true). After 5 tool_uses, only the last one
// (index 6) was buffered. The 7 content_block_stops that followed all got
// buffered as "mismatch" until the matching stop (index 6) finally triggered
// the flush — emitting the last tool_use along with 7 stale stops. The
// earlier 4 tool_uses lost their starts and deltas entirely; Claude Code
// saw stop events with no preceding starts and rejected the stream.
//
// Post-fix: each new tool_use start flushes the previous one, and stops for
// non-matching indices pass through immediately. Every tool_use's start and
// deltas reach the client.
func TestFixAnthropicImages_StreamMultipleToolUsesInThinking(t *testing.T) {
	p := &FixAnthropicImages{}
	ctx := &engine.PipelineContext{}

	// Mirror the upstream trace that triggered the bug: thinking block,
	// then 5 tool_uses (indices 2-6), then all content_block_stops in
	// order. Ends with [DONE] (engine synthesizes one because the upstream
	// omits message_stop). Each tool_use's content_block_stop arrives after
	// all tool_use starts, matching the upstream's actual emission order.
	events := []engine.SSEChunk{
		// Thinking block start
		{EventType: "content_block_start", Data: []byte(`{"index":0,"content_block":{"type":"thinking","thinking":""}}`)},
		// A handful of thinking deltas
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"thinking_delta","thinking":"The "}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"thinking_delta","thinking":"user "}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"thinking_delta","thinking":"wants X"}}`)},
		// 5 nested tool_uses — start, delta pairs, all in a row with no stops between them
		{EventType: "content_block_start", Data: []byte(`{"index":2,"content_block":{"type":"tool_use","id":"tu_2","name":"Read"}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":3,"content_block":{"type":"tool_use","id":"tu_3","name":"Glob"}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":4,"content_block":{"type":"tool_use","id":"tu_4","name":"Grep"}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":4,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":5,"content_block":{"type":"tool_use","id":"tu_5","name":"Bash"}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":5,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":6,"content_block":{"type":"tool_use","id":"tu_6","name":"Write"}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":6,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
		// All content_block_stops arrive in order: thinking, then each tool_use.
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":2}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":3}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":4}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":5}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":6}`)},
		// Engine synthesizes [DONE] after the upstream closes without one
		{EventType: "", Data: []byte("[DONE]")},
	}

	var results []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		results = append(results, out)
	}

	// Concatenate every output data field — both raw-SSE flushes and
	// framed pass-throughs — into one blob we can search.
	allOut := strings.Builder{}
	for _, r := range results {
		allOut.Write(r.Data)
	}
	combined := allOut.String()

	// Each tool_use's id must appear in the output exactly once. If any of
	// the 5 tool_use starts were silently swallowed (the pre-fix bug), at
	// least one id would be missing entirely.
	for _, id := range []string{"tu_2", "tu_3", "tu_4", "tu_5", "tu_6"} {
		if c := strings.Count(combined, id); c != 1 {
			t.Errorf("expected exactly 1 occurrence of %q in output, got %d. Combined output:\n%s", id, c, combined)
		}
	}

	// Each tool_use must appear as both a content_block_start and a
	// content_block_stop, with no orphan stops (the original bug's
	// symptom). Count appearances of each index inside event/data frames.
	for _, idx := range []int{2, 3, 4, 5, 6} {
		// Each tool_use must contribute one start and one stop event to the
		// output. The plugin's output for these events takes two forms:
		//   - tool_use starts/deltas inside the buffer are flushed as raw SSE
		//     "event: content_block_start\ndata: {...\"content_block\":{\"type\":\"tool_use\"...}}"
		//   - tool_use stops that triggered the flush are appended as raw SSE
		//     "event: content_block_stop\ndata: {\"index\":N}"
		// For a tool_use that gets flushed via a later start (not its own
		// stop), its stop is emitted later as a pass-through chunk whose data
		// is just `{"index":N}` — the engine then wraps that as the SSE event
		// when writing to the wire.
		//
		// Look for the tool_use start — must appear in raw-SSE flush form.
		startMarker := fmt.Sprintf("\"index\":%d,\"content_block\":{\"type\":\"tool_use\"", idx)
		startCount := strings.Count(combined, startMarker)
		if startCount != 1 {
			t.Errorf("index %d: expected 1 tool_use start, got %d", idx, startCount)
		}

		// The stop must appear at least once in the output. It can take
		// either form (raw-SSE flush form for the matching stop, or just
		// `{"index":N}` pass-through form for stops that arrive after the
		// tool_use was already flushed). The matching stop for index 6
		// triggers its flush and produces the raw-SSE form; stops for
		// 2..5 are pass-throughs and appear as raw `{"index":N}`.
		stopMarkerRaw := fmt.Sprintf("event: content_block_stop\ndata: {\"index\":%d}", idx)
		stopMarkerPassThru := fmt.Sprintf("{\"index\":%d}", idx)
		rawCount := strings.Count(combined, stopMarkerRaw)
		passThruCount := strings.Count(combined, stopMarkerPassThru)
		if rawCount+passThruCount < 1 {
			t.Errorf("index %d: expected at least 1 tool_use stop in output, got raw=%d passthrough=%d", idx, rawCount, passThruCount)
		}
	}

	// The thinking stop (index 0) is a pass-through (mismatch with the
	// buffered tool_use index). It must reach the client as `{"index":0}`.
	thinkingStopMarker := "{\"index\":0}"
	if c := strings.Count(combined, thinkingStopMarker); c < 1 {
		t.Errorf("expected thinking stop (index 0) in output, got %d occurrences", c)
	}
}
