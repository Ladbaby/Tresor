package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// ---------------- Gemini2Responses ----------------

func TestGemini2Responses_TransformRequest_Basic(t *testing.T) {
	p := &Gemini2Responses{}

	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Hello"}}},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{{"text": "You are helpful."}},
		},
		"generationConfig": map[string]interface{}{
			"maxOutputTokens": 256,
			"temperature":     0.5,
		},
	})

	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gpt-5:generateContent", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-resp"}}

	newReq, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	// Final URL should be /v1/responses after both hops.
	if newReq.URL.Path != "/v1/responses" {
		t.Fatalf("expected /v1/responses, got %s", newReq.URL.Path)
	}
	if newReq.Header.Get("Authorization") != "Bearer sk-resp" {
		t.Fatalf("expected Bearer auth, got %q", newReq.Header.Get("Authorization"))
	}

	var responsesReq map[string]interface{}
	if err := json.Unmarshal(newBody, &responsesReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if responsesReq["model"] != "gpt-5" {
		t.Fatalf("expected model gpt-5, got %v", responsesReq["model"])
	}
	// systemInstruction must have been promoted to Responses "instructions".
	if responsesReq["instructions"] != "You are helpful." {
		t.Fatalf("expected instructions 'You are helpful.', got %v", responsesReq["instructions"])
	}
	// input must be a slice of Responses input items (one user message).
	inputRaw, ok := responsesReq["input"].([]interface{})
	if !ok || len(inputRaw) != 1 {
		t.Fatalf("expected 1 input item, got %v", responsesReq["input"])
	}
	userItem, _ := inputRaw[0].(map[string]interface{})
	if userItem["role"] != "user" || userItem["content"] != "Hello" {
		t.Fatalf("expected user input item 'Hello', got %v", userItem)
	}
}

func TestGemini2Responses_TransformRequest_Stream(t *testing.T) {
	p := &Gemini2Responses{}

	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Stream please"}}},
		},
	})

	// streamGenerateContent in the path → stream=true must propagate to Responses.
	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gpt-5:streamGenerateContent", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-resp"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var responsesReq map[string]interface{}
	_ = json.Unmarshal(newBody, &responsesReq)
	if responsesReq["stream"] != true {
		t.Fatalf("expected stream=true for :streamGenerateContent path, got %v", responsesReq["stream"])
	}
}

func TestGemini2Responses_TransformResponse_JSON(t *testing.T) {
	p := &Gemini2Responses{}

	// A Responses API JSON response that the inner transformers will convert
	// back to a Gemini generateContentResponse.
	respBody := []byte(`{
		"id": "resp_1",
		"object": "response",
		"status": "completed",
		"model": "gpt-5",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "Hello there"}]
		}],
		"usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
	}`)

	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(resp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var gem map[string]interface{}
	if err := json.Unmarshal(out, &gem); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, string(out))
	}
	candidates, _ := gem["candidates"].([]interface{})
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0].(map[string]interface{})
	content, _ := c["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].(map[string]interface{})["text"] != "Hello there" {
		t.Fatalf("expected text 'Hello there', got %v", parts)
	}
	if c["finishReason"] != "STOP" {
		t.Fatalf("expected STOP finishReason, got %v", c["finishReason"])
	}
	if gem["modelVersion"] != "gpt-5" {
		t.Fatalf("expected modelVersion gpt-5, got %v", gem["modelVersion"])
	}
}

func TestGemini2Responses_TransformStreamChunk_TextDelta(t *testing.T) {
	p := &Gemini2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	// First, simulate the response.created event so the inner Responses->Chat
	// transformer initializes its state (response.id, model, etc.).
	createdChunk := engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"response":{"id":"resp_1","model":"gpt-5"}}`),
	}
	if _, err := p.TransformStreamChunk(createdChunk, ctx); err != nil {
		t.Fatalf("created: %v", err)
	}
	// response.in_progress so the inner transformer emits the role chunk.
	inProgress := engine.SSEChunk{EventType: "response.in_progress", Data: []byte(`{}`)}
	if _, err := p.TransformStreamChunk(inProgress, ctx); err != nil {
		t.Fatalf("in_progress: %v", err)
	}

	// Now a text delta — should round-trip into a Gemini text part.
	delta := engine.SSEChunk{
		EventType: "response.output_text.delta",
		Data:      []byte(`{"delta":"Hi "}`),
	}
	out, err := p.TransformStreamChunk(delta, ctx)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatalf("expected non-empty output chunk")
	}
	// The data: line should contain a Gemini generateContentResponse with text "Hi ".
	if !strings.Contains(string(out.Data), `"Hi "`) {
		t.Fatalf("expected output to contain 'Hi ', got: %s", string(out.Data))
	}
	if !strings.Contains(string(out.Data), `"role":"model"`) {
		t.Fatalf("expected output to contain model role, got: %s", string(out.Data))
	}
	// Must be wrapped as SSE data: line.
	if !bytes.HasPrefix(out.Data, []byte("data: ")) {
		t.Fatalf("expected data: prefix, got: %s", string(out.Data))
	}
}

// TestGemini2Responses_TransformStreamChunk_DropsDone verifies that the
// synthetic [DONE] marker (which the engine may inject for Responses
// streams) is dropped before reaching the Gemini client — Gemini has no
// equivalent terminator.
func TestGemini2Responses_TransformStreamChunk_DropsDone(t *testing.T) {
	p := &Gemini2Responses{}
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: []byte("[DONE]")}, &engine.PipelineContext{Variables: make(map[string]interface{})})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("expected empty output for [DONE], got: %s", string(out.Data))
	}
}

// TestGemini2Responses_TransformRequest_ThinkingConfig verifies that a
// Gemini client that enables thinking via generationConfig.thinkingConfig
// ends up with reasoning.effort on the Responses request, so the upstream
// model actually produces reasoning content to return.
func TestGemini2Responses_TransformRequest_ThinkingConfig(t *testing.T) {
	p := &Gemini2Responses{}
	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]interface{}{{"text": "Solve x^2=4"}}},
		},
		"generationConfig": map[string]interface{}{
			"thinkingConfig": map[string]interface{}{
				"includeThoughts": true,
				"thinkingBudget":  8192,
			},
		},
	})
	req, _ := http.NewRequest("POST", "http://example.com/v1beta/models/gpt-5:generateContent", bytes.NewReader(body))
	ctx := &engine.PipelineContext{TargetDownstream: &engine.Downstream{APIKey: "sk-test"}}

	_, newBody, err := p.TransformRequest(req, body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var responsesReq map[string]interface{}
	if err := json.Unmarshal(newBody, &responsesReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reasoning, ok := responsesReq["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected reasoning object on Responses request, got %v", responsesReq["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort 'high' (8192 budget), got %v", reasoning["effort"])
	}
}

// TestGemini2Responses_TransformResponse_Reasoning verifies that a Responses
// API response containing reasoning items is mapped all the way back to
// Gemini thought parts (parts[].thought: true). This is the inverse of the
// request-side thinkingConfig mapping.
func TestGemini2Responses_TransformResponse_Reasoning(t *testing.T) {
	p := &Gemini2Responses{}
	respBody := []byte(`{
		"id": "resp_123",
		"object": "response",
		"status": "completed",
		"model": "gpt-5",
		"output": [
			{
				"type": "reasoning",
				"summary": [
					{"type": "summary_text", "text": "Let me think..."}
				]
			},
			{"type": "message", "role": "assistant", "content": [
				{"type": "output_text", "text": "x = 2 or -2"}
			]}
		]
	}`)
	httpResp := &http.Response{StatusCode: 200, Header: http.Header{}}
	httpResp.Header.Set("Content-Type", "application/json")

	out, err := p.TransformResponse(httpResp, respBody, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var gem map[string]interface{}
	if err := json.Unmarshal(out, &gem); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	candidates, _ := gem["candidates"].([]interface{})
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	parts, _ := candidates[0].(map[string]interface{})["content"].(map[string]interface{})["parts"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (answer + thought), got %d", len(parts))
	}
	// Gemini2OpenAI emits content first, then thought parts.
	answer, _ := parts[0].(map[string]interface{})
	if answer["text"] != "x = 2 or -2" {
		t.Fatalf("expected first part text 'x = 2 or -2', got %v", answer["text"])
	}
	thought, _ := parts[1].(map[string]interface{})
	if thought["thought"] != true {
		t.Fatalf("expected second part to be a thought, got %v", thought)
	}
	if thought["text"] != "Let me think..." {
		t.Fatalf("expected thought text 'Let me think...', got %v", thought["text"])
	}
}

// TestGemini2Responses_TransformStreamChunk_ReasoningSummaryDelta verifies
// the streaming end of the reasoning path: a Responses
// response.reasoning_summary_text.delta event must reach the Gemini client
// as a generateContentResponse with parts[].thought: true.
func TestGemini2Responses_TransformStreamChunk_ReasoningSummaryDelta(t *testing.T) {
	p := &Gemini2Responses{}
	ctx := &engine.PipelineContext{Variables: make(map[string]interface{})}

	// Seed response.id/model state.
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.created",
		Data:      []byte(`{"response":{"id":"resp_123","model":"gpt-5"}}`),
	}, ctx)

	// A reasoning summary delta must reach the client as a thought part.
	out, err := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.reasoning_summary_text.delta",
		Data:      []byte(`{"delta":"Thinking aloud..."}`),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(string(out.Data), `"thought":true`) {
		t.Fatalf("expected thought:true in streamed reasoning part, got: %s", string(out.Data))
	}
	if !strings.Contains(string(out.Data), `"Thinking aloud..."`) {
		t.Fatalf("expected reasoning text to be carried through, got: %s", string(out.Data))
	}
}
