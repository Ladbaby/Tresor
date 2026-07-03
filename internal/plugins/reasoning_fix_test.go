package plugins

import (
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// TestAnthropic2Gemini_StreamingNoUsageMetadata verifies the streaming code
// does not panic when Gemini sends a final chunk without usageMetadata.
func TestAnthropic2Gemini_StreamingNoUsageMetadata(t *testing.T) {
	p := &Anthropic2Gemini{}
	// A chunk with no usageMetadata that has a finishReason — this used to
	// crash with a nil-pointer panic on the unauthenticated resp.UsageMetadata
	// dereference inside TransformStreamChunk.
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	_, err := p.TransformStreamChunk(engine.SSEChunk{Data: body}, ctx)
	if err != nil {
		t.Fatalf("transform stream chunk: %v", err)
	}
}

// TestAnthropic2Gemini_StreamingThinkingBlockShape verifies the thinking
// content_block_start shape matches the Anthropic SDK's discriminated-union
// schema: top-level `type` field plus `content_block.type:"thinking"` and
// `content_block.thinking` (empty string). The Vercel AI SDK / Cherry Studio
// Zod schema requires a discriminator `type` at the top level (matching
// the SSE event name) for every chunk. The previous code omitted the top
// level `type` field and added `signature` to the content_block, which
// the schema (z.object({type, thinking})) does not accept.
func TestAnthropic2Gemini_StreamingThinkingBlockShape(t *testing.T) {
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// First chunk: a Gemini chunk that opens the message (returns message_start).
	openChunk := []byte(`{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`)
	_, err := p.TransformStreamChunk(engine.SSEChunk{Data: openChunk}, ctx)
	if err != nil {
		t.Fatalf("first transform: %v", err)
	}

	// Second chunk: the actual thought part. Emit content_block_start with
	// the SDK-acceptable shape: top-level type field + content_block with
	// type and thinking (no signature — that comes via signature_delta).
	thoughtChunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking text","thought":true}]}}]}`)
	chunk, err := p.TransformStreamChunk(engine.SSEChunk{Data: thoughtChunk}, ctx)
	if err != nil {
		t.Fatalf("thought transform: %v", err)
	}
	out := string(chunk.Data)
	// Top-level `type` discriminator MUST be present in the data JSON —
	// the Vercel AI SDK / Anthropic SDK Zod schema reads it from the data
	// field, not just from the SSE `event:` line.
	if !strings.Contains(out, `"type":"content_block_start"`) {
		t.Fatalf("expected top-level type:content_block_start in data, got %s", out)
	}
	// content_block.thinking is required (the schema rejects the event if
	// it's missing or if extra fields like `signature` are present).
	// Check for the content_block object as a whole (JSON keys marshal
	// alphabetically so order isn't reliable).
	if !strings.Contains(out, `"content_block":{`) {
		t.Fatalf("expected content_block object, got %s", out)
	}
	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("expected content_block type:thinking, got %s", out)
	}
	if !strings.Contains(out, `"thinking":""`) {
		t.Fatalf("expected empty thinking field in content_block, got %s", out)
	}
	// The content_block_start event itself must NOT carry the actual
	// thinking text — it goes in a thinking_delta instead. We check by
	// looking for the start event object (which only contains type+thinking
	// with empty thinking).
	if strings.Contains(out, `"content_block":{"thinking":"thinking text"`) {
		t.Fatalf("content_block_start must not carry the thinking text, got %s", out)
	}
	// A thinking_delta with the actual text must appear in the same chunk
	// (or a follow-up chunk) — otherwise the thinking text is dropped.
	if !strings.Contains(out, `"delta":{"thinking":"thinking text","type":"thinking_delta"}`) {
		t.Fatalf("expected thinking_delta to carry the actual text, got %s", out)
	}
}

// TestAnthropic2OpenAI_StreamingReasoningContent verifies reasoning_content
// in OpenAI Chat Completions deltas surfaces as Anthropic thinking events
// with the SDK-acceptable start shape (top-level type, content_block.type,
// content_block.thinking).
func TestAnthropic2OpenAI_StreamingReasoningContent(t *testing.T) {
	p := &Anthropic2OpenAI{}
	// Final chunk with reasoning_content + finish_reason so the thinking
	// block is closed (with signature_delta) within a single observable
	// TransformStreamChunk call.
	body := []byte(`{"id":"chatcmpl-x","model":"o1","choices":[{"index":0,"delta":{"reasoning_content":"Let me think"},"finish_reason":"stop"}]}`)
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	chunk, err := p.TransformStreamChunk(engine.SSEChunk{Data: body}, ctx)
	if err != nil {
		t.Fatalf("transform stream chunk: %v", err)
	}
	out := string(chunk.Data)
	if !strings.Contains(out, `"type":"content_block_start"`) {
		t.Fatalf("expected top-level type:content_block_start, got %s", out)
	}
	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("expected thinking content block, got %s", out)
	}
	if !strings.Contains(out, `"thinking":""`) {
		t.Fatalf("expected empty thinking field on start, got %s", out)
	}
	if !strings.Contains(out, `"type":"thinking_delta"`) {
		t.Fatalf("expected thinking_delta, got %s", out)
	}
	if !strings.Contains(out, `"type":"signature_delta"`) {
		t.Fatalf("expected signature_delta to close the thinking block, got %s", out)
	}
	if !strings.Contains(out, `"type":"content_block_stop"`) {
		t.Fatalf("expected content_block_stop, got %s", out)
	}
}

// TestAnthropic2OpenAI_NonStreamReasoningContent verifies reasoning_content
// in a non-streaming OpenAI Chat Completion response becomes a thinking
// content block in the Anthropic response.
func TestAnthropic2OpenAI_NonStreamReasoningContent(t *testing.T) {
	p := &Anthropic2OpenAI{}
	body := []byte(`{
		"id":"chatcmpl-x",
		"model":"o1",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"hello","reasoning_content":"reasoning text"},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}
	}`)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	out, err := p.TransformResponse(resp, body, ctx)
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}
	if !strings.Contains(string(out), `"type":"thinking"`) {
		t.Fatalf("expected thinking content block, got %s", string(out))
	}
	if !strings.Contains(string(out), `"thinking":"reasoning text"`) {
		t.Fatalf("expected reasoning text in thinking block, got %s", string(out))
	}
}

// TestAnthropic2Responses_NonStreamReasoningItem verifies a `reasoning` item
// in a non-streaming OpenAI Responses API output becomes an Anthropic
// thinking content block.
func TestAnthropic2Responses_NonStreamReasoningItem(t *testing.T) {
	p := &Anthropic2Responses{}
	body := []byte(`{
		"id":"resp_x",
		"model":"o1",
		"status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking text"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
		],
		"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}
	}`)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	out, err := p.TransformResponse(resp, body, ctx)
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}
	if !strings.Contains(string(out), `"type":"thinking"`) {
		t.Fatalf("expected thinking content block, got %s", string(out))
	}
	if !strings.Contains(string(out), `"thinking":"thinking text"`) {
		t.Fatalf("expected thinking text, got %s", string(out))
	}
}

// TestAnthropic2Responses_StreamingReasoningEvents verifies reasoning events
// in a Responses API stream surface as Anthropic thinking/thinking_delta events
// with the SDK-acceptable start shape (top-level type, content_block.type,
// content_block.thinking).
func TestAnthropic2Responses_StreamingReasoningEvents(t *testing.T) {
	p := &Anthropic2Responses{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Reasoning output item added — Responses API uses {item: {type, id}, output_index}
	chunk1 := []byte(`{"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`)
	out1, err := p.TransformStreamChunk(engine.SSEChunk{EventType: "response.output_item.added", Data: chunk1}, ctx)
	if err != nil {
		t.Fatalf("reasoning added: %v", err)
	}
	out1Str := string(out1.Data)
	if !strings.Contains(out1Str, `"type":"content_block_start"`) {
		t.Fatalf("expected top-level type:content_block_start, got %s", out1Str)
	}
	if !strings.Contains(out1Str, `"type":"thinking"`) {
		t.Fatalf("expected content_block with type:thinking, got %s", out1Str)
	}
	if !strings.Contains(out1Str, `"thinking":""`) {
		t.Fatalf("expected empty thinking field, got %s", out1Str)
	}

	// Reasoning summary text delta
	chunk2 := []byte(`{"delta":"reasoning chunk"}`)
	out2, err := p.TransformStreamChunk(engine.SSEChunk{EventType: "response.reasoning_summary_text.delta", Data: chunk2}, ctx)
	if err != nil {
		t.Fatalf("reasoning delta: %v", err)
	}
	if !strings.Contains(string(out2.Data), `"type":"thinking_delta"`) {
		t.Fatalf("expected thinking_delta, got %s", string(out2.Data))
	}

	// Reasoning output item done — must emit signature_delta + content_block_stop.
	// The Responses API uses item.id as the key.
	chunk3 := []byte(`{"output_index":0,"item":{"id":"rs_1"}}`)
	out3, err := p.TransformStreamChunk(engine.SSEChunk{EventType: "response.output_item.done", Data: chunk3}, ctx)
	if err != nil {
		t.Fatalf("reasoning done: %v", err)
	}
	if !strings.Contains(string(out3.Data), `"type":"signature_delta"`) {
		t.Fatalf("expected signature_delta, got %s", string(out3.Data))
	}
	if !strings.Contains(string(out3.Data), `"type":"content_block_stop"`) {
		t.Fatalf("expected content_block_stop, got %s", string(out3.Data))
	}
}

// TestAnthropic2Gemini_StreamingFirstTextNotDropped verifies that the very
// first text chunk emitted by Gemini after a thinking block (or after
// message_start when no thinking is emitted) carries both the
// content_block_start AND the text_delta for the current chunk's text. The
// previous implementation returned immediately after writing the start event,
// so the model output "Hi there! 👋 ..." but the client only saw the second
// chunk's content ("there! 👋 ...") — losing the leading words.
func TestAnthropic2Gemini_StreamingFirstTextNotDropped(t *testing.T) {
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// First chunk: open the message.
	openChunk := []byte(`{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: openChunk}, ctx); err != nil {
		t.Fatalf("first transform: %v", err)
	}

	// Second chunk: a non-thinking text part — the first one of the response.
	// This must produce both content_block_start AND a text_delta carrying
	// "Hi " in the SAME SSE chunk (so the leading words aren't dropped).
	firstText := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`)
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: firstText}, ctx)
	if err != nil {
		t.Fatalf("text transform: %v", err)
	}
	combined := string(out.Data)
	if !strings.Contains(combined, `"type":"content_block_start"`) {
		t.Fatalf("expected content_block_start in first-text chunk, got %s", combined)
	}
	if !strings.Contains(combined, `"type":"text_delta"`) {
		t.Fatalf("expected text_delta in first-text chunk (would otherwise drop the leading text), got %s", combined)
	}
	if !strings.Contains(combined, `"text":"Hi "`) {
		t.Fatalf("expected text_delta to carry the current chunk's text \"Hi \", got %s", combined)
	}

	// Third chunk: more text — should produce only a text_delta (no new
	// content_block_start) since the text block is already open.
	moreText := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"there!"}]}}]}`)
	out2, err := p.TransformStreamChunk(engine.SSEChunk{Data: moreText}, ctx)
	if err != nil {
		t.Fatalf("more text: %v", err)
	}
	if strings.Count(string(out2.Data), `"type":"content_block_start"`) != 0 {
		t.Fatalf("second text chunk should not re-open the text block, got %s", string(out2.Data))
	}
	if !strings.Contains(string(out2.Data), `"text":"there!"`) {
		t.Fatalf("expected second chunk to carry \"there!\", got %s", string(out2.Data))
	}
}

// TestAnthropic2Gemini_StreamingFirstTextAfterThinkingNotDropped verifies the
// same fix when a thinking block came before the first text part.
func TestAnthropic2Gemini_StreamingFirstTextAfterThinkingNotDropped(t *testing.T) {
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	openChunk := []byte(`{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: openChunk}, ctx); err != nil {
		t.Fatalf("open: %v", err)
	}

	thoughtChunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true}]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: thoughtChunk}, ctx); err != nil {
		t.Fatalf("thought: %v", err)
	}

	// First text after thinking. Must produce signature_delta + content_block_stop
	// (close thinking) + content_block_start (open text) + text_delta (carry text)
	// all in the same SSE chunk.
	firstText := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`)
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: firstText}, ctx)
	if err != nil {
		t.Fatalf("first text: %v", err)
	}
	combined := string(out.Data)
	for _, want := range []string{
		`"type":"signature_delta"`,
		`"type":"content_block_stop"`,
		`"type":"content_block_start"`,
		`"type":"text_delta"`,
		`"text":"Hi "`,
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("expected %q in first-text-after-thinking chunk, got %s", want, combined)
		}
	}
}

// TestAnthropic2Gemini_StreamingMessageDeltaAlwaysHasUsage verifies that the
// message_delta event always carries a `usage` object (even when the upstream
// Gemini chunk has no usageMetadata). The Vercel AI SDK's
// AnthropicMessageDelta Zod schema requires `usage` to be present as an
// object — omitting it produced "AI_TypeValidationError: Value:
// {\"delta\":{...}} — expected: object, path: [usage]".
func TestAnthropic2Gemini_StreamingMessageDeltaAlwaysHasUsage(t *testing.T) {
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	// Open the message and walk through to finishReason with NO usageMetadata.
	openChunk := []byte(`{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: openChunk}, ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	textChunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: textChunk}, ctx); err != nil {
		t.Fatalf("text: %v", err)
	}

	// Final chunk: NO usageMetadata. Must still emit message_delta with a
	// usage object present.
	final := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}`)
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: final}, ctx)
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if !strings.Contains(string(out.Data), `"type":"message_delta"`) {
		t.Fatalf("expected message_delta on finish, got %s", string(out.Data))
	}
	if !strings.Contains(string(out.Data), `"usage":{`) {
		t.Fatalf("message_delta must carry usage object even without usageMetadata, got %s", string(out.Data))
	}
}

// TestAnthropic2Gemini_StreamingTextBlockClosedOnFinish verifies that the
// text content_block is closed with a matching content_block_stop before
// message_delta on finish. Without the stop, the Anthropic SDK either
// truncates the trailing text or rejects the stream — the user reported
// "ending words are missing" with the previous code.
func TestAnthropic2Gemini_StreamingTextBlockClosedOnFinish(t *testing.T) {
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}

	openChunk := []byte(`{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: openChunk}, ctx); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Two text chunks so the text block is well-established.
	text1 := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: text1}, ctx); err != nil {
		t.Fatalf("text1: %v", err)
	}
	text2 := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"there!"}]}}]}`)
	if _, err := p.TransformStreamChunk(engine.SSEChunk{Data: text2}, ctx); err != nil {
		t.Fatalf("text2: %v", err)
	}

	// Final chunk must close the text block BEFORE the message_delta — and
	// it must carry the closing delta's text. Without the stop, the
	// trailing "there!" gets dropped at the schema validation step.
	final := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":" bye."}]},"finishReason":"STOP"}]}`)
	out, err := p.TransformStreamChunk(engine.SSEChunk{Data: final}, ctx)
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	combined := string(out.Data)
	// The closing text_delta (" bye.") must be present.
	if !strings.Contains(combined, `"text":" bye."`) {
		t.Fatalf("expected closing text_delta to carry \" bye.\", got %s", combined)
	}
	// The text block must be closed with a content_block_stop.
	if !strings.Contains(combined, `"type":"content_block_stop"`) {
		t.Fatalf("expected content_block_stop for text block on finish, got %s", combined)
	}
	// The message_delta must follow.
	if !strings.Contains(combined, `"type":"message_delta"`) {
		t.Fatalf("expected message_delta on finish, got %s", combined)
	}
}