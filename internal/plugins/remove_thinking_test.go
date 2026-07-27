package plugins

import (
	"encoding/json"
	"net/http"
	"testing"

	"tresor/internal/engine"
)

// ===== Non-streaming =====

func TestRemoveThinking_OpenAI_NonStreaming_RemovesReasoningContent(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"id":"chatcmpl-1","object":"chat.completion",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":"The answer is 42.",
				"reasoning_content":"Let me think... 6*9=54, 7*6=42."
			}
		}]
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	out := string(newBody)

	mustContain(t, out, `"content":"The answer is 42."`)
	mustNotContain(t, out, "reasoning_content")
	mustNotContain(t, out, "Let me think")
}

func TestRemoveThinking_OpenAI_NonStreaming_NoChangeWhenNoReasoning(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(newBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := got["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "hi" {
		t.Errorf("content lost: %v", msg)
	}
}

func TestRemoveThinking_Anthropic_NonStreaming_DropsThinkingBlock(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[
			{"type":"thinking","thinking":"internal reasoning","signature":"sig-abc"},
			{"type":"text","text":"Hello there."}
		]
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	out := string(newBody)

	mustContain(t, out, `"text":"Hello there."`)
	mustNotContain(t, out, `"type":"thinking"`)
	mustNotContain(t, out, "internal reasoning")
	mustNotContain(t, out, "sig-abc")
}

func TestRemoveThinking_Anthropic_NonStreaming_StripsThinkingTokens(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"id":"msg_2","type":"message","role":"assistant",
		"content":[{"type":"text","text":"done"}],
		"usage":{"input_tokens":10,"output_tokens":5,"thinking_tokens":42}
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	out := string(newBody)

	mustNotContain(t, out, "thinking_tokens")
	// Other usage fields preserved
	var got map[string]interface{}
	_ = json.Unmarshal(newBody, &got)
	usage := got["usage"].(map[string]interface{})
	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Errorf("other usage fields lost: %v", usage)
	}
}

func TestRemoveThinking_Anthropic_NonStreaming_NoChangeWhenNoThinking(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"id":"msg_3","type":"message","role":"assistant",
		"content":[{"type":"text","text":"plain"}],
		"stop_reason":"end_turn"
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	mustContain(t, string(newBody), `"text":"plain"`)
}

func TestRemoveThinking_Responses_NonStreaming_DropsReasoningItems(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"id":"resp_1","object":"response",
		"output":[
			{"id":"rs_0","type":"reasoning","summary":[{"type":"summary_text","text":"chain of thought"}]},
			{"id":"msg_0","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
			{"id":"fc_0","type":"function_call","name":"x","arguments":"{}"}
		]
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	out := string(newBody)

	mustNotContain(t, out, `"type":"reasoning"`)
	mustNotContain(t, out, "chain of thought")
	mustContain(t, out, `"text":"hi"`)
	mustContain(t, out, `"type":"function_call"`)
}

func TestRemoveThinking_Gemini_NonStreaming_DropsThoughtParts(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`{
		"candidates":[{
			"index":0,
			"content":{
				"role":"model",
				"parts":[
					{"text":"internal thought","thought":true},
					{"text":"Visible answer."}
				]
			}
		}]
	}`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	out := string(newBody)

	mustNotContain(t, out, "internal thought")
	mustNotContain(t, out, `"thought":true`)
	mustContain(t, out, `"text":"Visible answer."`)
}

func TestRemoveThinking_UnknownFormat_PassesThrough(t *testing.T) {
	p := &RemoveThinking{}
	body := []byte(`not even json`)
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(newBody) != string(body) {
		t.Errorf("garbage body should pass through unchanged")
	}
}

func TestRemoveThinking_EmptyBody_PassesThrough(t *testing.T) {
	p := &RemoveThinking{}
	resp := &http.Response{Header: http.Header{}}

	newBody, err := p.TransformResponse(resp, []byte{}, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(newBody) != 0 {
		t.Errorf("empty body should remain empty, got %q", newBody)
	}
}

// ===== Streaming - OpenAI =====

func TestRemoveThinking_OpenAI_Stream_RemovesReasoningDelta(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{Data: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking step","content":"answer"}}]}`)}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	got := string(out.Data)
	mustContain(t, got, `"content":"answer"`)
	mustNotContain(t, got, "reasoning_content")
	mustNotContain(t, got, "thinking step")
}

func TestRemoveThinking_OpenAI_Stream_PreservesContentOnly(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`)}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	mustContain(t, string(out.Data), `"content":"hello"`)
}

func TestRemoveThinking_OpenAI_Stream_ResetsOnDone(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// Prime state
	_, _ = p.TransformStreamChunk(engine.SSEChunk{Data: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"x"}}]}`)}, ctx)
	if p.detectedFormat != "openai" {
		t.Fatalf("format not detected: %q", p.detectedFormat)
	}

	// [DONE] should reset
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: []byte(`[DONE]`)}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if string(out.Data) != "[DONE]" {
		t.Errorf("[DONE] should pass through verbatim, got %q", out.Data)
	}
	if p.detectedFormat != "" {
		t.Errorf("state not reset, detectedFormat=%q", p.detectedFormat)
	}
}

// ===== Streaming - Anthropic =====

func TestRemoveThinking_Anthropic_Stream_DropsThinkingBlockEvents(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// 1. message_start: detect format
	_, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"m","role":"assistant"}}`),
	}, ctx)

	// 2. content_block_start for text block: pass through
	out, _ := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}, ctx)
	if len(out.Data) == 0 {
		t.Errorf("text content_block_start should pass through, got empty")
	}

	// 3. content_block_start for thinking block: dropped, state tracked
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`),
	}, ctx)
	if len(out.Data) != 0 {
		t.Errorf("thinking content_block_start should be dropped, got %q", out.Data)
	}
	if !p.anthropicInsideThinking {
		t.Errorf("anthropicInsideThinking should be true")
	}

	// 4. thinking_delta: dropped
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"reasoning..."}}`),
	}, ctx)
	if len(out.Data) != 0 {
		t.Errorf("thinking_delta should be dropped, got %q", out.Data)
	}

	// 5. content_block_stop for the thinking block: dropped, state cleared
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_stop",
		Data:      []byte(`{"type":"content_block_stop","index":1}`),
	}, ctx)
	if len(out.Data) != 0 {
		t.Errorf("matching content_block_stop for thinking should be dropped, got %q", out.Data)
	}
	if p.anthropicInsideThinking {
		t.Errorf("anthropicInsideThinking should be false after stop")
	}

	// 6. message_delta: pass through
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`),
	}, ctx)
	if len(out.Data) == 0 {
		t.Errorf("message_delta should pass through")
	}

	// 7. message_stop: pass through, reset
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_stop",
		Data:      []byte(`{"type":"message_stop"}`),
	}, ctx)
	if len(out.Data) == 0 {
		t.Errorf("message_stop should pass through")
	}
	if p.detectedFormat != "" {
		t.Errorf("state should reset on message_stop, detectedFormat=%q", p.detectedFormat)
	}
}

func TestRemoveThinking_Anthropic_Stream_DropsSignatureDelta(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// Open thinking block
	_, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
	}, ctx)

	// signature_delta should be dropped (it's inside a thinking block)
	out, _ := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}`),
	}, ctx)
	if len(out.Data) != 0 {
		t.Errorf("signature_delta inside thinking block should be dropped, got %q", out.Data)
	}
}

func TestRemoveThinking_Anthropic_Stream_StripsThinkingTokensInMessageDelta(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10,"thinking_tokens":99}}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	got := string(out.Data)
	mustNotContain(t, got, "thinking_tokens")
	mustContain(t, got, `"output_tokens":10`)
}

func TestRemoveThinking_Anthropic_Stream_PreservesSiblingStopWhileInsideThinking(t *testing.T) {
	// Edge case: a text block opened BEFORE the thinking block sends its
	// stop event WHILE we're inside the thinking block. That stop must
	// pass through (otherwise the SDK sees a stop with no preceding start).
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// Open text block (index 0)
	_, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}, ctx)
	// Open thinking block (index 1)
	_, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_start",
		Data:      []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`),
	}, ctx)

	// Stop for the text block (index 0) arrives while we're inside the
	// thinking block (index 1). This stop must pass through.
	out, _ := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_stop",
		Data:      []byte(`{"type":"content_block_stop","index":0}`),
	}, ctx)
	if len(out.Data) == 0 {
		t.Errorf("stop for sibling block should pass through, got empty")
	}
	// Thinking state must still be set
	if !p.anthropicInsideThinking {
		t.Errorf("anthropicInsideThinking should remain true")
	}

	// Stop for the thinking block (index 1) is the one that ends the drop
	out, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "content_block_stop",
		Data:      []byte(`{"type":"content_block_stop","index":1}`),
	}, ctx)
	if len(out.Data) != 0 {
		t.Errorf("matching stop for thinking should be dropped, got %q", out.Data)
	}
}

// ===== Streaming - OpenAI Responses =====

func TestRemoveThinking_Responses_Stream_DropsReasoningEvents(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	for _, ev := range []string{
		"response.reasoning",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
	} {
		out, err := p.TransformStreamChunk(engine.SSEChunk{
			EventType: ev,
			Data:      []byte(`{"foo":"bar"}`),
		}, ctx)
		if err != nil {
			t.Fatalf("transform %s: %v", ev, err)
		}
		if len(out.Data) != 0 {
			t.Errorf("event %q should be dropped, got %q", ev, out.Data)
		}
	}
}

func TestRemoveThinking_Responses_Stream_TracksReasoningItemAdded(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// output_item.added for reasoning: dropped, index tracked
	out, err := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.output_item.added",
		Data:      []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_0","type":"reasoning","summary":[]}}`),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("reasoning output_item.added should be dropped, got %q", out.Data)
	}
	if _, ok := p.responsesReasoningOutputIdx[0]; !ok {
		t.Errorf("reasoning index 0 should be tracked, got %v", p.responsesReasoningOutputIdx)
	}

	// output_item.done for the same reasoning index: dropped via tracker
	out, err = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.output_item.done",
		Data:      []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_0","type":"reasoning"}}`),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("matching reasoning output_item.done should be dropped, got %q", out.Data)
	}
}

func TestRemoveThinking_Responses_Stream_StripsFromCompletedEvent(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	// Track a reasoning index
	_, _ = p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.output_item.added",
		Data:      []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_0","type":"reasoning","summary":[]}}`),
	}, ctx)

	// response.completed with mixed output: reasoning items stripped
	completed := `{"type":"response.completed","response":{"id":"r1","output":[
		{"id":"rs_0","type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"id":"msg_0","type":"message","content":[{"type":"output_text","text":"hi"}]}
	]}}`
	out, err := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.completed",
		Data:      []byte(completed),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	got := string(out.Data)
	mustNotContain(t, got, `"type":"reasoning"`)
	mustNotContain(t, got, "think")
	mustContain(t, got, `"text":"hi"`)

	// After response.completed, state should be reset.
	if p.detectedFormat != "" {
		t.Errorf("state should reset after response.completed, detectedFormat=%q", p.detectedFormat)
	}
}

func TestRemoveThinking_Responses_Stream_PreservesNonReasoningOutputItemAdded(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	out, err := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "response.output_item.added",
		Data:      []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message"}}`),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Data) == 0 {
		t.Errorf("non-reasoning output_item.added should pass through")
	}
}

// ===== Streaming - Gemini =====

func TestRemoveThinking_Gemini_Stream_DropsThoughtParts(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		Data: []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[
			{"text":"internal","thought":true},
			{"text":"visible"}
		]}}]}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	got := string(out.Data)
	mustNotContain(t, got, "internal")
	mustNotContain(t, got, `"thought":true`)
	mustContain(t, got, `"text":"visible"`)
}

func TestRemoveThinking_Gemini_Stream_PreservesTextParts(t *testing.T) {
	p := &RemoveThinking{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		Data: []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}]}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	// No thought parts → no rewrite → data passed through unchanged
	mustContain(t, string(out.Data), `"text":"hello"`)
}

// ===== Format detection =====

func TestRemoveThinking_DetectFormat_OpenAI(t *testing.T) {
	if got := detectResponseFormat([]byte(`{"choices":[{"message":{"content":"x"}}]}`)); got != "openai" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectFormat_Anthropic(t *testing.T) {
	// Typed content blocks
	if got := detectResponseFormat([]byte(`{"content":[{"type":"text","text":"x"}]}`)); got != "anthropic" {
		t.Errorf("typed blocks: got %q", got)
	}
	// Or stop_reason presence
	if got := detectResponseFormat([]byte(`{"content":[],"stop_reason":"end_turn"}`)); got != "anthropic" {
		t.Errorf("stop_reason: got %q", got)
	}
}

func TestRemoveThinking_DetectFormat_Responses(t *testing.T) {
	if got := detectResponseFormat([]byte(`{"output":[{"type":"message"}]}`)); got != "openai_responses" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectFormat_Gemini(t *testing.T) {
	if got := detectResponseFormat([]byte(`{"candidates":[{"content":{"parts":[]}}]}`)); got != "gemini" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectFormat_Unknown(t *testing.T) {
	if got := detectResponseFormat([]byte(`{"foo":"bar"}`)); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := detectResponseFormat([]byte(``)); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := detectResponseFormat([]byte(`not json`)); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestRemoveThinking_DetectStreamingFormat_Anthropic(t *testing.T) {
	if got := detectStreamingFormatFromChunk(engine.SSEChunk{EventType: "message_start"}); got != "anthropic" {
		t.Errorf("got %q", got)
	}
	if got := detectStreamingFormatFromChunk(engine.SSEChunk{EventType: "content_block_delta"}); got != "anthropic" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectStreamingFormat_Responses(t *testing.T) {
	if got := detectStreamingFormatFromChunk(engine.SSEChunk{EventType: "response.created"}); got != "openai_responses" {
		t.Errorf("got %q", got)
	}
	if got := detectStreamingFormatFromChunk(engine.SSEChunk{EventType: "response.reasoning_summary_text.delta"}); got != "openai_responses" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectStreamingFormat_OpenAI(t *testing.T) {
	chunk := engine.SSEChunk{Data: []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)}
	if got := detectStreamingFormatFromChunk(chunk); got != "openai" {
		t.Errorf("got %q", got)
	}
}

func TestRemoveThinking_DetectStreamingFormat_Gemini(t *testing.T) {
	chunk := engine.SSEChunk{Data: []byte(`{"candidates":[{"content":{"parts":[]}}]}`)}
	if got := detectStreamingFormatFromChunk(chunk); got != "gemini" {
		t.Errorf("got %q", got)
	}
}
