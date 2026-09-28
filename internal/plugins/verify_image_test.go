package plugins

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// Regression: when an Anthropic tool_result carries an image (e.g. the
// `read` tool returning a screenshot), the Responses-API
// `function_call_output.output` must be promoted to an array of
// input_text / input_image parts — matching the reference pi client.
// A string-only output silently drops the image, so ChatGPT receives no
// image bytes at all and "cannot see the image".
func TestAnthropic2Responses_ToolResultImagePromotedToInputImage(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"model": "gpt-5.6-luna",
		"messages": [
			{"role": "user", "content": "What is in this file?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tu_1", "name": "read", "input": {"path": "chart.png"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": [
					{"type": "text", "text": "(see attached image)"},
					{"type": "image", "source": {
						"type": "base64", "media_type": "image/png", "data": "Y2hhcnQtZGF0YQ=="
					}}
				]}
			]}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	input, _ := result["input"].([]interface{})

	// Locate the function_call_output item.
	var out interface{}
	found := false
	for _, item := range input {
		im, _ := item.(map[string]interface{})
		if im["type"] == "function_call_output" {
			out = im["output"]
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no function_call_output item in input")
	}

	// Must be an array, not a plain string — otherwise the image is dropped.
	parts, ok := out.([]interface{})
	if !ok {
		t.Fatalf("expected output to be an array when the result has an image, got %T: %v", out, out)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d: %v", len(parts), parts)
	}
	textPart := parts[0].(map[string]interface{})
	if textPart["type"] != "input_text" || textPart["text"] != "(see attached image)" {
		t.Fatalf("expected first part input_text, got %v", textPart)
	}
	imgPart := parts[1].(map[string]interface{})
	if imgPart["type"] != "input_image" {
		t.Fatalf("expected second part input_image, got %v", imgPart)
	}
	url, _ := imgPart["image_url"].(string)
	if url != "data:image/png;base64,Y2hhcnQtZGF0YQ==" {
		t.Fatalf("expected data url 'data:image/png;base64,Y2hhcnQtZGF0YQ==', got %q", url)
	}
}

// A text-only tool_result must stay a plain string (no spurious array),
// preserving the prior behavior for the common case.
func TestAnthropic2Responses_ToolResultTextOnlyStaysString(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"model": "gpt-5.6-luna",
		"messages": [
			{"role": "user", "content": "Weather?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": [
					{"type": "text", "text": "22°C"}
				]}
			]}
		],
		"stream": false
	}`)
	req, _ := http.NewRequest("POST", "http://example.com/v1/messages", nil)
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}
	newReq, _, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(newReq.Body).Decode(&result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	input, _ := result["input"].([]interface{})
	for _, item := range input {
		im, _ := item.(map[string]interface{})
		if im["type"] == "function_call_output" {
			out, ok := im["output"].(string)
			if !ok {
				t.Fatalf("expected text-only tool_result output to stay a string, got %T", im["output"])
			}
			if !strings.Contains(out, "22") {
				t.Fatalf("expected output to contain the text, got %q", out)
			}
			return
		}
	}
	t.Fatalf("no function_call_output item in input")
}
