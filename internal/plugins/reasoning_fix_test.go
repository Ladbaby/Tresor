package plugins

import (
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// geminiOpenChunk is the minimal "open the message" chunk Gemini emits
// first. Used to drive Anthropic2Gemini tests past the message_start gate.
const geminiOpenChunk = `{"modelVersion":"gemini-2.0-flash","candidates":[{"content":{"role":"model","parts":[]}}]}`

// driveGemini runs a sequence of Gemini chunks through one Anthropic2Gemini
// stream transformer state machine and returns the concatenated Data across
// all chunks. Tests share a single openChunk via geminiOpenChunk so the
// state advances naturally across calls.
// ponytail: five tests repeat the open+parts+final sequence verbatim —
// one helper, one assertion list per scenario.
func driveGemini(t *testing.T, chunks ...string) string {
	t.Helper()
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	var out strings.Builder
	for _, c := range chunks {
		chunk, err := p.TransformStreamChunk(engine.SSEChunk{Data: []byte(c)}, ctx)
		if err != nil {
			t.Fatalf("transform chunk: %v", err)
		}
		out.Write(chunk.Data)
	}
	return out.String()
}

func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Fatalf("expected %q in output, got:\n%s", n, haystack)
		}
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("expected %q NOT in output, got:\n%s", needle, haystack)
	}
}

// TestAnthropic2Gemini_StreamingNoUsageMetadata: a final chunk with finishReason
// but no usageMetadata used to crash with a nil deref on resp.UsageMetadata.
func TestAnthropic2Gemini_StreamingNoUsageMetadata(t *testing.T) {
	driveGemini(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`,
	)
}

func TestAnthropic2Gemini_StreamingThinkingBlockShape(t *testing.T) {
	out := driveGemini(t,
		geminiOpenChunk,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking text","thought":true}]}}]}`,
	)
	mustContain(t, out,
		`"type":"content_block_start"`,
		`"content_block":{`,
		`"type":"thinking"`,
		`"thinking":""`,
		`"delta":{"thinking":"thinking text","type":"thinking_delta"}`,
	)
	mustNotContain(t, out, `"content_block":{"thinking":"thinking text"`)
}

func TestAnthropic2Gemini_StreamingFirstTextNotDropped(t *testing.T) {
	// First text chunk: emit start + delta in the SAME chunk so "Hi "
	// isn't dropped on the freshly-opened text block.
	// Second text chunk: block is open — must NOT re-emit start.
	p := &Anthropic2Gemini{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	send := func(data string) string {
		out, err := p.TransformStreamChunk(engine.SSEChunk{Data: []byte(data)}, ctx)
		if err != nil {
			t.Fatalf("transform: %v", err)
		}
		return string(out.Data)
	}
	first := send(geminiOpenChunk)
	first += send(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`)
	mustContain(t, first,
		`"type":"content_block_start"`,
		`"type":"text_delta"`,
		`"text":"Hi "`,
	)
	more := send(`{"candidates":[{"content":{"role":"model","parts":[{"text":"there!"}]}}]}`)
	if strings.Count(more, `"type":"content_block_start"`) != 0 {
		t.Fatalf("second text chunk re-opened the text block:\n%s", more)
	}
	mustContain(t, more, `"text":"there!"`)
}

func TestAnthropic2Gemini_StreamingFirstTextAfterThinkingNotDropped(t *testing.T) {
	out := driveGemini(t,
		geminiOpenChunk,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`,
	)
	mustContain(t, out,
		`"type":"signature_delta"`,
		`"type":"content_block_stop"`,
		`"type":"content_block_start"`,
		`"type":"text_delta"`,
		`"text":"Hi "`,
	)
}

func TestAnthropic2Gemini_StreamingMessageDeltaAlwaysHasUsage(t *testing.T) {
	out := driveGemini(t,
		geminiOpenChunk,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}`,
	)
	// Vercel AI SDK's message_delta Zod schema requires usage to be a
	// present object even when Gemini omits usageMetadata.
	mustContain(t, out, `"type":"message_delta"`, `"usage":{`)
}

func TestAnthropic2Gemini_StreamingTextBlockClosedOnFinish(t *testing.T) {
	out := driveGemini(t,
		geminiOpenChunk,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi "}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"there!"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":" bye."}]},"finishReason":"STOP"}]}`,
	)
	mustContain(t, out,
		`"text":" bye."`,
		`"type":"content_block_stop"`,
		`"type":"message_delta"`,
	)
}

func TestAnthropic2OpenAI_StreamingReasoningContent(t *testing.T) {
	// Single-chunk stream with reasoning_content + finish_reason — the
	// thinking block must open, emit a delta, and close (signature+stop)
	// all in one TransformStreamChunk call.
	p := &Anthropic2OpenAI{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	body := []byte(`{"id":"chatcmpl-x","model":"o1","choices":[{"index":0,"delta":{"reasoning_content":"Let me think"},"finish_reason":"stop"}]}`)
	chunk, err := p.TransformStreamChunk(engine.SSEChunk{Data: body}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	mustContain(t, string(chunk.Data),
		`"type":"content_block_start"`,
		`"type":"thinking"`,
		`"thinking":""`,
		`"type":"thinking_delta"`,
		`"type":"signature_delta"`,
		`"type":"content_block_stop"`,
	)
}

func TestAnthropic2OpenAI_NonStreamReasoningContent(t *testing.T) {
	// Non-streaming OpenAI Chat response carries reasoning_content in
	// the assistant message → thinking block in Anthropic output.
	p := &Anthropic2OpenAI{}
	body := []byte(`{
		"id":"chatcmpl-x","model":"o1",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"reasoning text"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}
	}`)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "application/json")
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	out, err := p.TransformResponse(resp, body, ctx)
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}
	mustContain(t, string(out), `"type":"thinking"`, `"thinking":"reasoning text"`)
}

func TestAnthropic2Responses_NonStreamReasoningItem(t *testing.T) {
	// Responses API output[0] is a `reasoning` item — must produce a
	// thinking block in the Anthropic response.
	p := &Anthropic2Responses{}
	body := []byte(`{
		"id":"resp_x","model":"o1","status":"completed",
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
	mustContain(t, string(out), `"type":"thinking"`, `"thinking":"thinking text"`)
}

func TestAnthropic2Responses_StreamingReasoningEvents(t *testing.T) {
	// Three Responses-API events across the same stream: output_item.added
	// (opens thinking block), reasoning_summary_text.delta (delta),
	// output_item.done (signature + stop).
	p := &Anthropic2Responses{}
	ctx := &engine.PipelineContext{Variables: map[string]interface{}{}}
	send := func(event string, data string) string {
		out, err := p.TransformStreamChunk(engine.SSEChunk{EventType: event, Data: []byte(data)}, ctx)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		return string(out.Data)
	}
	added := send("response.output_item.added", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`)
	mustContain(t, added,
		`"type":"content_block_start"`,
		`"type":"thinking"`,
		`"thinking":""`,
	)
	delta := send("response.reasoning_summary_text.delta", `{"delta":"reasoning chunk"}`)
	mustContain(t, delta, `"type":"thinking_delta"`)
	done := send("response.output_item.done", `{"output_index":0,"item":{"id":"rs_1"}}`)
	mustContain(t, done, `"type":"signature_delta"`, `"type":"content_block_stop"`)
}
