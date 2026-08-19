package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tresor/internal/inspect"
)

// TestCalculateBackoff verifies the exponential backoff curve, the max-delay
// cap, and that the result is always at least the base delay.
func TestCalculateBackoff(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		minMS   int // expected minimum delay (excluding jitter)
		maxMS   int // expected maximum delay (base + 25% jitter)
	}{
		{name: "attempt 1", attempt: 1, minMS: 500, maxMS: 625},  // 500 + up to 25%
		{name: "attempt 2", attempt: 2, minMS: 1000, maxMS: 1250}, // 1000 + up to 25%
		{name: "attempt 3", attempt: 3, minMS: 2000, maxMS: 2500}, // 2000 + up to 25%
		{name: "attempt 4 capped", attempt: 4, minMS: 4000, maxMS: 5000},
		{name: "attempt 5 capped", attempt: 5, minMS: 8000, maxMS: 10000},
		{name: "attempt 6 capped at 16s", attempt: 6, minMS: 16000, maxMS: 20000},
		{name: "attempt 10 still capped", attempt: 10, minMS: 16000, maxMS: 20000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateBackoff(tt.attempt)
			minDur := time.Duration(tt.minMS) * time.Millisecond
			maxDur := time.Duration(tt.maxMS) * time.Millisecond
			if got < minDur {
				t.Errorf("CalculateBackoff(%d) = %v, want >= %v", tt.attempt, got, minDur)
			}
			if got > maxDur {
				t.Errorf("CalculateBackoff(%d) = %v, want <= %v", tt.attempt, got, maxDur)
			}
		})
	}
}

// TestCalculateBackoff_Jitter verifies that repeated calls produce different
// delays (jitter is non-deterministic) — guards against accidentally
// removing the rand.Float64() call.
func TestCalculateBackoff_Jitter(t *testing.T) {
	first := CalculateBackoff(3)
	different := false
	for i := 0; i < 10; i++ {
		if CalculateBackoff(3) != first {
			different = true
			break
		}
	}
	if !different {
		t.Errorf("expected jitter to produce varying delays over 10 calls, all returned %v", first)
	}
}

// ---- bufferedWriter ----

// TestBufferedWriter_PassThroughBeforeFlush confirms that writes before Flush
// are captured in the buffer and WriteHeader captures the status without
// committing it to the underlying writer.
func TestBufferedWriter_PassThroughBeforeFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(200)
	bw.Write([]byte("hello "))
	bw.Write([]byte("world"))

	if rec.Code != http.StatusOK {
		// httptest.NewRecorder starts at 200 but should not see the buffered 200 yet
		// — and Code is only updated by WriteHeader on the underlying writer.
		// In practice httptest.NewRecorder initializes Code=200, so we can't distinguish
		// from this. Inspect the body instead.
		t.Logf("rec.Code=%d (expected to remain default 200; real signal is the body)", rec.Code)
	}

	if rec.Body.Len() != 0 {
		t.Errorf("expected no body before Flush, got %q", rec.Body.String())
	}
	if bw.IsFlushed() {
		t.Error("expected IsFlushed()=false before Flush()")
	}
}

// TestBufferedWriter_FlushCommitsBufferAndHeader confirms that Flush writes
// the buffered status, headers, and body to the underlying writer, and
// subsequent writes pass through.
func TestBufferedWriter_FlushCommitsBufferAndHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(http.StatusAccepted)
	bw.Write([]byte("payload"))
	bw.Flush()

	if rec.Code != http.StatusAccepted {
		t.Errorf("expected status %d after Flush, got %d", http.StatusAccepted, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("expected Content-Type=text/event-stream, got %q", got)
	}
	if rec.Body.String() != "payload" {
		t.Errorf("expected body=payload, got %q", rec.Body.String())
	}
	if !bw.IsFlushed() {
		t.Error("expected IsFlushed()=true after Flush()")
	}

	// Subsequent writes pass through directly.
	bw.Write([]byte("-more"))
	if rec.Body.String() != "payload-more" {
		t.Errorf("expected post-flush write to pass through, body=%q", rec.Body.String())
	}
}

// TestBufferedWriter_DoubleFlushIsNoop confirms Flush is idempotent.
func TestBufferedWriter_DoubleFlushIsNoop(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.WriteHeader(200)
	bw.Write([]byte("first"))
	bw.Flush()
	bw.Flush() // should be a no-op

	if rec.Body.String() != "first" {
		t.Errorf("expected body=first (no duplication), got %q", rec.Body.String())
	}
}

// TestBufferedWriter_HeaderPassthrough confirms Header() reaches the
// underlying writer so header mutations are visible immediately.
func TestBufferedWriter_HeaderPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("X-Test", "yes")

	if got := rec.Header().Get("X-Test"); got != "yes" {
		t.Errorf("expected underlying writer header X-Test=yes, got %q", got)
	}
}

// ---- IsOpenAIChatEmpty ----

func TestIsOpenAIChatEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "non-empty content",
			body: `{"choices":[{"message":{"content":"hello"}}]}`,
			want: false,
		},
		{
			name: "empty choices array",
			body: `{"choices":[]}`,
			want: true,
		},
		{
			name: "choices with null content",
			body: `{"choices":[{"message":{"content":null}}]}`,
			want: true,
		},
		{
			name: "choices with empty string content",
			body: `{"choices":[{"message":{"content":""}}]}`,
			want: true,
		},
		{
			name: "tool_call counts as content",
			body: `{"choices":[{"message":{"tool_calls":[{"id":"x"}]}}]}`,
			want: false,
		},
		{
			name: "refusal counts as content",
			body: `{"choices":[{"message":{"refusal":"cannot"}}]}`,
			want: false,
		},
		{
			name: "thinking-only delta treated as empty",
			body: `{"choices":[{"delta":{"content":null,"reasoning_content":"thinking..."}}]}`,
			want: true,
		},
		{
			name: "non-empty delta counts as content",
			body: `{"choices":[{"delta":{"content":"hi"}}]}`,
			want: false,
		},
		{
			name: "missing choices field treated as empty (unmarshal succeeds with empty slice)",
			body: `{"error":"x"}`,
			want: true,
		},
		{
			name: "malformed JSON treated as non-empty (defensive — can't parse, assume not empty)",
			body: `not-json`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOpenAIChatEmpty([]byte(tt.body))
			if got != tt.want {
				t.Errorf("IsOpenAIChatEmpty(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// ---- IsAnthropicEmpty ----

func TestIsAnthropicEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "non-empty text block",
			body: `{"content":[{"type":"text","text":"hello"}]}`,
			want: false,
		},
		{
			name: "empty content array",
			body: `{"content":[]}`,
			want: true,
		},
		{
			name: "tool_use counts as content",
			body: `{"content":[{"type":"tool_use","id":"x","name":"f","input":{}}]}`,
			want: false,
		},
		{
			name: "thinking-only block treated as empty",
			body: `{"content":[{"type":"thinking","thinking":"pondering..."}]}`,
			want: true,
		},
		{
			name: "mixed thinking + text — text wins",
			body: `{"content":[{"type":"thinking","thinking":"pondering..."},{"type":"text","text":"hi"}]}`,
			want: false,
		},
		{
			name: "missing content field treated as empty (unmarshal succeeds with empty slice)",
			body: `{"error":"x"}`,
			want: true,
		},
		{
			// REGRESSION: count_tokens responses carry only {"input_tokens":N}
			// (no content field). The parser still reports them as empty —
			// the GATE is the path check in shouldRetry, not the parser.
			// See count-tokens-trigger-empty-response.txt.
			name: "count_tokens shape (input_tokens only) treated as empty",
			body: `{"input_tokens":7802}`,
			want: true,
		},
		{
			name: "malformed JSON treated as non-empty (defensive — can't parse, assume not empty)",
			body: `not-json`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAnthropicEmpty([]byte(tt.body))
			if got != tt.want {
				t.Errorf("IsAnthropicEmpty(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// ---- IsOpenAIResponsesEmpty ----

func TestIsOpenAIResponsesEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "non-empty output_text",
			body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`,
			want: false,
		},
		{
			name: "empty output array",
			body: `{"output":[]}`,
			want: true,
		},
		{
			name: "function_call counts as content",
			body: `{"output":[{"type":"function_call","name":"f","arguments":"{}"}]}`,
			want: false,
		},
		{
			name: "reasoning-only treated as empty",
			body: `{"output":[{"type":"reasoning","summary":"thinking"}]}`,
			want: true,
		},
		{
			name: "message with empty content array treated as empty",
			body: `{"output":[{"type":"message","content":[]}]}`,
			want: true,
		},
		{
			name: "missing output field treated as empty (unmarshal succeeds with empty slice)",
			body: `{"error":"x"}`,
			want: true,
		},
		{
			name: "malformed JSON treated as non-empty (defensive — can't parse, assume not empty)",
			body: `not-json`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOpenAIResponsesEmpty([]byte(tt.body))
			if got != tt.want {
				t.Errorf("IsOpenAIResponsesEmpty(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// ---- IsGeminiEmpty ----

func TestIsGeminiEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "non-empty text part",
			body: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
			want: false,
		},
		{
			name: "empty candidates array",
			body: `{"candidates":[]}`,
			want: true,
		},
		{
			name: "functionCall counts as content",
			body: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`,
			want: false,
		},
		{
			name: "thinking part only — treated as empty",
			body: `{"candidates":[{"content":{"parts":[{"thought":true,"text":"pondering"}]}}]}`,
			want: true,
		},
		{
			name: "mixed thinking + real text — real text wins",
			body: `{"candidates":[{"content":{"parts":[{"thought":true,"text":"ponder"},{"text":"hi"}]}}]}`,
			want: false,
		},
		{
			name: "candidate with no parts is treated as empty",
			body: `{"candidates":[{"content":{"parts":[]}}]}`,
			want: true,
		},
		{
			name: "missing candidates field treated as empty (unmarshal succeeds with empty slice)",
			body: `{"error":"x"}`,
			want: true,
		},
		{
			name: "malformed JSON treated as non-empty (defensive — can't parse, assume not empty)",
			body: `not-json`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsGeminiEmpty([]byte(tt.body))
			if got != tt.want {
				t.Errorf("IsGeminiEmpty(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// ---- IsStreamContentLine ----

func TestIsStreamContentLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		format string
		want   bool
	}{
		// anthropic
		{name: "anthropic text_delta", line: `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, format: "anthropic", want: true},
		{name: "anthropic input_json_delta", line: `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`, format: "anthropic", want: true},
		{name: "anthropic tool_use", line: `data: {"type":"content_block_start","content_block":{"type":"tool_use"}}`, format: "anthropic", want: true},
		{name: "anthropic thinking_delta only — not content", line: `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}`, format: "anthropic", want: false},

		// openai
		{name: "openai non-empty content", line: `data: {"choices":[{"delta":{"content":"hi"}}]}`, format: "openai", want: true},
		{name: "openai null content — not content", line: `data: {"choices":[{"delta":{"content":null}}]}`, format: "openai", want: false},
		{name: "openai empty string content — not content", line: `data: {"choices":[{"delta":{"content":""}}]}`, format: "openai", want: false},
		{name: "openai tool_calls", line: `data: {"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`, format: "openai", want: true},
		{name: "openai refusal", line: `data: {"choices":[{"delta":{"refusal":"no"}}]}`, format: "openai", want: true},
		{name: "openai reasoning_content — not content", line: `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, format: "openai", want: false},

		// openai_responses
		{name: "openai_responses output_text delta event", line: `event: response.output_text.delta`, format: "openai_responses", want: true},
		{name: "openai_responses function_call_arguments delta event", line: `event: response.function_call_arguments.delta`, format: "openai_responses", want: true},
		{name: "openai_responses reasoning_summary delta — not content", line: `event: response.reasoning_summary_text.delta`, format: "openai_responses", want: false},
		{name: "openai_responses raw text payload", line: `data: {"text":"hi"}`, format: "openai_responses", want: true},

		// gemini
		{name: "gemini text part", line: `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, format: "gemini", want: true},
		{name: "gemini functionCall part", line: `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}]}`, format: "gemini", want: true},
		{name: "gemini thinking part only — not content", line: `data: {"candidates":[{"content":{"parts":[{"thought":true,"text":"ponder"}]}}]}`, format: "gemini", want: false},

		// unknown format
		{name: "unknown format returns false", line: `data: {"foo":"bar"}`, format: "unknown", want: false},

		// missing data: prefix still parsed by helper
		{name: "no data prefix openai", line: `{"choices":[{"delta":{"content":"hi"}}]}`, format: "openai", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsStreamContentLine(tt.line, tt.format)
			if got != tt.want {
				t.Errorf("IsStreamContentLine(%q, %q) = %v, want %v", tt.line, tt.format, got, tt.want)
			}
		})
	}
}

// ---- Engine.shouldRetry / isResponseEmpty ----

func TestEngine_ShouldRetry_OnlyRetries200WithEmptyBody(t *testing.T) {
	eng := &Engine{}
	respOK := &http.Response{StatusCode: 200}

	// 200 + empty + generation endpoint → retry
	if !eng.shouldRetry(respOK, []byte(`{"choices":[]}`), "openai", "/v1/chat/completions") {
		t.Error("200 with empty OpenAI body should retry")
	}
	// 200 + non-empty + generation endpoint → no retry
	if eng.shouldRetry(respOK, []byte(`{"choices":[{"message":{"content":"hi"}}]}`), "openai", "/v1/chat/completions") {
		t.Error("200 with non-empty body should NOT retry")
	}
	// 500 + empty → no retry (HTTP errors are the client's responsibility)
	resp500 := &http.Response{StatusCode: 500}
	if eng.shouldRetry(resp500, []byte(`{"choices":[]}`), "openai", "/v1/chat/completions") {
		t.Error("500 must NOT trigger retry regardless of body")
	}
	// 429 + empty → no retry
	resp429 := &http.Response{StatusCode: 429}
	if eng.shouldRetry(resp429, []byte(`{"choices":[]}`), "openai", "/v1/chat/completions") {
		t.Error("429 must NOT trigger retry (client handles rate limits)")
	}
}

// TestEngine_ShouldRetry_SkipsNonGenerationEndpoints pins down the
// retry_on_empty eligibility gate. retry_on_empty is generation-only;
// utility endpoints like /v1/messages/count_tokens can legitimately return
// 200 with bodies that look "empty" to the parser (e.g.
// {"input_tokens":7802}). retrying them produces a spurious 502.
func TestEngine_ShouldRetry_SkipsNonGenerationEndpoints(t *testing.T) {
	eng := &Engine{}
	respOK := &http.Response{StatusCode: 200}

	tests := []struct {
		name     string
		body     string
		format   string
		path     string
		wantRetry bool
	}{
		{
			name: "count_tokens skipped (anthropic format, no content)",
			body: `{"input_tokens":7802}`,
			format: "anthropic",
			path: "/v1/messages/count_tokens",
			wantRetry: false,
		},
		{
			name: "bare /count_tokens path also skipped",
			body: `{"input_tokens":42}`,
			format: "anthropic",
			path: "/count_tokens",
			wantRetry: false,
		},
		{
			name: "models listing endpoint skipped",
			body: `{"data":[]}`,
			format: "openai",
			path: "/v1/models",
			wantRetry: false,
		},
		{
			name: "embeddings endpoint skipped",
			body: `{"data":[]}`,
			format: "openai",
			path: "/v1/embeddings",
			wantRetry: false,
		},
		{
			name: "gemini model listing skipped",
			body: `{"models":[]}`,
			format: "gemini",
			path: "/v1beta/models",
			wantRetry: false,
		},
		{
			name: "unknown custom path skipped (conservative default)",
			body: `{"choices":[]}`,
			format: "openai",
			path: "/v1/custom-thing",
			wantRetry: false,
		},
		{
			name: "generation path with empty content still retries",
			body: `{"content":[]}`,
			format: "anthropic",
			path: "/v1/messages",
			wantRetry: true,
		},
		{
			name: "generation path with non-empty content does not retry",
			body: `{"content":[{"type":"text","text":"hi"}]}`,
			format: "anthropic",
			path: "/v1/messages",
			wantRetry: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eng.shouldRetry(respOK, []byte(tt.body), tt.format, tt.path)
			if got != tt.wantRetry {
				t.Errorf("shouldRetry(%q, %q) = %v, want %v", tt.path, tt.body, got, tt.wantRetry)
			}
		})
	}
}

func TestIsGenerationEndpoint(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Generation endpoints (allowed)
		{"/v1/chat/completions", true},
		{"/v1/completions", true},
		{"/v1/messages", true},
		{"/v1/responses", true},
		// Bare forms (no /v1 prefix) — also allowed since the engine
		// sometimes sees paths without the prefix.
		{"/chat/completions", true},
		{"/messages", true},
		// Utility endpoints (skipped)
		{"/v1/messages/count_tokens", false},
		{"/v1/models", false},
		{"/v1/embeddings", false},
		{"/v1beta/models", false},
		{"/v1beta/models/foo:generateContent", false},
		{"/count_tokens", false},
		{"", false},
		{"/", false},
		// Unknown custom path — conservative default
		{"/v1/custom-thing", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isGenerationEndpoint(tt.path); got != tt.want {
				t.Errorf("isGenerationEndpoint(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestEngine_IsResponseEmpty_DispatchesByFormat(t *testing.T) {
	eng := &Engine{}
	// unknown format → not empty (defensive default)
	if eng.isResponseEmpty([]byte(`{}`), "unknown-format") {
		t.Error("unknown format should default to non-empty")
	}
	// openai
	if !eng.isResponseEmpty([]byte(`{"choices":[]}`), "openai") {
		t.Error("empty openai response should be detected as empty")
	}
	// anthropic
	if !eng.isResponseEmpty([]byte(`{"content":[]}`), "anthropic") {
		t.Error("empty anthropic response should be detected as empty")
	}
	// openai_responses
	if !eng.isResponseEmpty([]byte(`{"output":[]}`), "openai_responses") {
		t.Error("empty openai_responses response should be detected as empty")
	}
	// gemini
	if !eng.isResponseEmpty([]byte(`{"candidates":[]}`), "gemini") {
		t.Error("empty gemini response should be detected as empty")
	}
}

func TestEngine_SetRetryOnEmpty_TogglesFlag(t *testing.T) {
	eng := &Engine{}
	eng.SetRetryOnEmpty(true)
	if !eng.retryOnEmpty {
		t.Error("SetRetryOnEmpty(true) should enable retryOnEmpty")
	}
	eng.SetRetryOnEmpty(false)
	if eng.retryOnEmpty {
		t.Error("SetRetryOnEmpty(false) should disable retryOnEmpty")
	}
}

// TestBufferedWriter_FlushEmptyBuffer confirms Flush with no writes still
// commits the captured status (so non-200 errors reach the client).
func TestBufferedWriter_FlushEmptyBuffer(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "application/json")
	bw.WriteHeader(http.StatusBadGateway)
	// no body writes
	bw.Flush()

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status %d, got %d", http.StatusBadGateway, rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rec.Body.String())
	}
}

// Ensure the bufio.Scanner default-buffer 64KB limit isn't relied on here;
// regression guard for the helper file's import set.
var _ = bytes.NewBuffer

// ---- Integration tests: streaming retry-on-empty end-to-end ----
//
// These exercise the engine's full streaming path with retry_on_empty
// enabled to verify the headers-aren't-lost bug fix and the retry-on-empty
// happy paths.

// makeStreamingDownstream returns an httptest server that emits a configurable
// SSE response. attempts is consulted by the test to count how many times the
// downstream was hit.
func makeStreamingDownstream(t *testing.T, attempts *int, sseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts != nil {
			*attempts++
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Write the SSE body in one shot; flush between events so the client
		// can detect content as soon as it arrives.
		for _, line := range strings.Split(sseBody, "\n") {
			if line == "" {
				continue
			}
			fmt.Fprint(w, line+"\n")
		}
		fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_PreservesHeaders is a
// regression test for the bug where, with retry_on_empty=true, the SSE
// Content-Type / Cache-Control / Connection headers were lost because
// WriteHeader was called before the headers were copied to the writer.
// Before the fix, the client received application/octet-stream (the Go
// default) instead of text/event-stream. After the fix, the downstream's
// SSE headers reach the client intact.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_PreservesHeaders(t *testing.T) {
	s := newTestStore(t)
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	var attempts int
	ts := makeStreamingDownstream(t, &attempts, sseBody)
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true) // <-- the bug was only on this path

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	// The fix must preserve the downstream's SSE headers.
	if got := w.Result().Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("expected Content-Type to start with text/event-stream, got %q", got)
	}
	if got := w.Result().Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("expected Cache-Control=no-cache, got %q", got)
	}
	if got := w.Result().Header.Get("Connection"); got != "keep-alive" {
		t.Errorf("expected Connection=keep-alive, got %q", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Errorf("expected SSE body to contain content, got %q", w.Body.String())
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_DisabledPathUnaffected
// verifies the disabled (default) path still gets SSE headers — guards
// against the header copy being moved into a branch that only runs when
// retry-on-empty is on.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_DisabledPathUnaffected(t *testing.T) {
	s := newTestStore(t)
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	ts := makeStreamingDownstream(t, nil, sseBody)
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	// retryOnEmpty NOT enabled (default)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if got := w.Result().Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("expected Content-Type=text/event-stream on disabled path, got %q", got)
	}
	if got := w.Result().Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("expected Cache-Control=no-cache on disabled path, got %q", got)
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_RetriesUntilContent
// verifies the happy path of the retry-on-empty feature for streaming:
// the first downstream response is empty (no real content), the gateway
// retries, and the second response succeeds.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_RetriesUntilContent(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		if attempts == 1 {
			// First attempt: thinking-only (treated as empty)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n")
		} else {
			// Second attempt: real content
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts != 2 {
		t.Errorf("expected downstream to be called twice (retry on empty), got %d", attempts)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Errorf("expected content from successful retry in body, got %q", w.Body.String())
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_ExhaustsRetriesAndErrors
// verifies that when all retries return reasoning-only (no real content),
// the gateway ultimately returns the upstream reasoning events to the
// client followed by an error message. With the streaming-preservation
// fix, reasoning tokens stream progressively to the client (so headers
// get committed as 200 on the first event); when retries are exhausted,
// the gateway appends an error body but cannot change the status code.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_ExhaustsRetriesAndErrors(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Always send thinking-only (empty) content.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	// retryMaxCount is 3, plus the initial attempt = 4 total.
	if attempts != 1+3 {
		t.Errorf("expected %d downstream calls (1 initial + 3 retries), got %d", 1+3, attempts)
	}
	// Reasoning events have been streamed progressively to the client,
	// so headers were committed as 200 on the first event. The error
	// body is appended after retries are exhausted. We accept 200
	// here because the fix prioritizes preserving the reasoning stream
	// the user can see; the alternative (dropping reasoning to enable
	// 502) was rejected as worse UX.
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (reasoning preserved, headers committed), got %d", w.Code)
	}
	respBody := w.Body.String()
	// The body should contain reasoning from the final attempt and
	// the empty-response error message appended by the engine.
	if !strings.Contains(respBody, "reasoning_content") {
		t.Errorf("expected body to contain reasoning events, got %q", respBody)
	}
	if !strings.Contains(strings.ToLower(respBody), "empty") {
		t.Errorf("expected body to mention 'empty' after retries exhausted, got %q", respBody)
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_RetriesUntilContent
// verifies the non-streaming retry path: empty body (HTTP 200, empty
// choices) → retry → second attempt returns content.
func TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_RetriesUntilContent(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if attempts == 1 {
			fmt.Fprint(w, `{"choices":[]}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}]}`)
		}
	}))
	defer ts.Close()
	// Register with api_formats=[openai] so the engine's empty-response detector
	// knows which JSON shape to parse.
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts != 2 {
		t.Errorf("expected downstream called twice (retry), got %d", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	choices, _ := result["choices"].([]interface{})
	if len(choices) == 0 {
		t.Fatalf("expected non-empty choices after retry, got %v", result)
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_DoesNotRetryOnNon200
// verifies that HTTP 500 / 429 responses are NEVER retried even when
// retry_on_empty is enabled — LLM client apps have their own retry
// semantics for HTTP errors.
func TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_DoesNotRetryOnNon200(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"upstream failed"}`)
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts != 1 {
		t.Errorf("expected 1 downstream call (no retry on 500), got %d", attempts)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 to pass through, got %d", w.Code)
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_CountTokensNotRetried
// is the regression test for the count_tokens false positive.
//
// Anthropic's POST /v1/messages/count_tokens returns
// `{"input_tokens":<int>}` with no `content` field. The parser
// (IsAnthropicEmpty) sees an empty content slice and reports the body
// as empty. Before the fix, retry_on_empty would retry the request
// three times and eventually return 502 "empty response after
// retries exhausted" — even though the downstream reply was correct.
//
// After the fix, retry_on_empty is gated on the request path: only
// generation endpoints are eligible. count_tokens is a utility
// endpoint and the body is passed through verbatim on the first try.
func TestEngine_HandleProxy_RetryOnEmpty_NonStreaming_CountTokensNotRetried(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Real Anthropic count_tokens payload shape.
		fmt.Fprint(w, `{"input_tokens":7802}`)
	}))
	defer ts.Close()
	// Anthropic-format downstream so IsAnthropicEmpty is the parser
	// that would (incorrectly) call this body empty.
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "anthropic")
	addOutputModelIDs(t, s, "ds1", "claude-sonnet-4-20250514")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true) // the bug only manifests with this enabled

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	// The downstream must be hit exactly once — the gate is the path,
	// not the parser, so the body is passed through.
	if attempts != 1 {
		t.Errorf("expected downstream called once (no retry on count_tokens), got %d", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	// The body must be delivered to the client verbatim.
	if got := strings.TrimSpace(w.Body.String()); got != `{"input_tokens":7802}` {
		t.Errorf("expected body to be passed through verbatim, got %q", got)
	}
	// And the response must NOT be a 502 gate.
	if strings.Contains(strings.ToLower(w.Body.String()), "empty response") {
		t.Errorf("expected no 502/empty-response error, got %q", w.Body.String())
	}
}

// TestCalculateBackoff_MonotonicBeforeCap verifies the exponential growth
// is monotonic for attempts below the cap (where the base delay itself
// doubles each attempt and jitter cannot overpower the growth).
func TestCalculateBackoff_MonotonicBeforeCap(t *testing.T) {
	// attempts 1..4: base 500ms, 1s, 2s, 4s. Even with up to 25% jitter,
	// attempt N's base is at least 2x attempt N-1's base, so the result is
	// strictly monotonic.
	prev := CalculateBackoff(1)
	for i := 2; i <= 5; i++ {
		cur := CalculateBackoff(i)
		if cur <= prev {
			t.Errorf("CalculateBackoff(%d)=%v should be > CalculateBackoff(%d)=%v", i, cur, i-1, prev)
		}
		prev = cur
	}
}

// TestCalculateBackoff_RespectsCap verifies the 16s cap. After enough
// doublings (500ms * 2^5 = 16s), the base should plateau.
func TestCalculateBackoff_RespectsCap(t *testing.T) {
	// 2^5 = 32 → cap kicks in. Both attempt 6 and 10 should be near 16s.
	d6 := CalculateBackoff(6)
	d10 := CalculateBackoff(10)
	// Both should be in [16000ms, 20000ms] (16s + up to 25% jitter).
	minD := 16 * time.Second
	maxD := 20 * time.Second
	if d6 < minD || d6 > maxD {
		t.Errorf("attempt 6 = %v, expected in [%v, %v]", d6, minD, maxD)
	}
	if d10 < minD || d10 > maxD {
		t.Errorf("attempt 10 = %v, expected in [%v, %v]", d10, minD, maxD)
	}
}

// ---- DetectStreamFormat ----

func TestDetectStreamFormat(t *testing.T) {
	tests := []struct {
		name  string
		chunk SSEChunk
		want  string
	}{
		{name: "anthropic by event", chunk: SSEChunk{EventType: "message_start"}, want: "anthropic"},
		{name: "anthropic content_block_delta", chunk: SSEChunk{EventType: "content_block_delta"}, want: "anthropic"},
		{name: "openai_responses by event", chunk: SSEChunk{EventType: "response.created"}, want: "openai_responses"},
		{name: "openai by payload", chunk: SSEChunk{Data: []byte(`{"choices":[{"delta":{"content":"x"}}]}`)}, want: "openai"},
		{name: "gemini by payload", chunk: SSEChunk{Data: []byte(`{"candidates":[{"content":{"parts":[]}}]}`)}, want: "gemini"},
		{name: "unknown payload", chunk: SSEChunk{Data: []byte(`{"foo":"bar"}`)}, want: ""},
		{name: "empty chunk", chunk: SSEChunk{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectStreamFormat(tt.chunk); got != tt.want {
				t.Errorf("DetectStreamFormat(%+v) = %q, want %q", tt.chunk, got, tt.want)
			}
		})
	}
}

// TestIsStreamContentLine_AnthropicStreamWithWrongSeed exercises Bug 2:
// when an OpenAI request is auto-translated to Anthropic, the seed format
// passed to the streaming handler is "openai", but the actual SSE data
// is in Anthropic format (content_block_delta with text_delta). With the
// bug, IsStreamContentLine would check for OpenAI markers in Anthropic
// data and return false (wrongly classifying the stream as empty).
//
// After Bug 2's fix, the handler confirms the format from the first
// chunk before calling IsStreamContentLine. This test asserts the two
// pieces work together: given an Anthropic chunk, the detected format
// is "anthropic", and IsStreamContentLine with "anthropic" correctly
// identifies text_delta as content (returns true).
func TestIsStreamContentLine_AnthropicStreamWithWrongSeed(t *testing.T) {
	// Simulate the chunk the streaming handler would see.
	chunk := SSEChunk{
		EventType: "content_block_delta",
		Data:      []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`),
	}

	// Step 1: on-the-fly detection (with seed "openai") — handler calls
	// DetectStreamFormat first; it should detect "anthropic" from the
	// event type.
	detected := DetectStreamFormat(chunk)
	if detected != "anthropic" {
		t.Fatalf("DetectStreamFormat = %q, want anthropic", detected)
	}

	// Step 2: with the correct format, IsStreamContentLine returns true
	// for content-bearing Anthropic events.
	if !IsStreamContentLine("data: "+string(chunk.Data), detected) {
		t.Error("Anthropic text_delta should be detected as content with correct format")
	}

	// Step 3 (regression check): with the wrong seed ("openai"), the
	// pre-Bug-2 code path would have returned false, misclassifying
	// the stream as empty. This shows why we need on-the-fly detection.
	if IsStreamContentLine("data: "+string(chunk.Data), "openai") {
		t.Error("sanity: Anthropic data with openai format should NOT detect content (proves why detection matters)")
	}
}

// ---- bufferedWriter.Drain ----

// TestBufferedWriter_Drain verifies that Drain() commits buffered bytes to
// the underlying writer WITHOUT switching to pass-through mode. Subsequent
// writes continue to be buffered; only an explicit Flush() commits the
// no-retry decision.
func TestBufferedWriter_Drain(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(200)
	bw.Write([]byte("first"))
	bw.Drain()

	if !strings.Contains(rec.Body.String(), "first") {
		t.Errorf("expected Drain to write buffered bytes, body=%q", rec.Body.String())
	}
	if rec.Body.String() != "first" {
		t.Errorf("expected body=first after Drain, got %q", rec.Body.String())
	}
	// After Drain, writer must still be in buffering mode.
	if bw.IsFlushed() {
		t.Error("Drain() must NOT switch to pass-through (IsFlushed should be false)")
	}

	// Subsequent writes are still buffered.
	bw.Write([]byte("-more"))
	if rec.Body.String() != "first" {
		t.Errorf("expected body still=first after Drain+Write, got %q (second write should be buffered)", rec.Body.String())
	}

	// Explicit Flush commits the rest.
	bw.Flush()
	if rec.Body.String() != "first-more" {
		t.Errorf("expected body=first-more after Flush, got %q", rec.Body.String())
	}
	if !bw.IsFlushed() {
		t.Error("IsFlushed should be true after Flush()")
	}
}

// TestBufferedWriter_DrainEmpty verifies that Drain is a no-op when the
// buffer is empty (no spurious flushes).
func TestBufferedWriter_DrainEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(200)
	// Don't write anything — just drain.
	bw.Drain()

	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body after Drain on empty buffer, got %q", rec.Body.String())
	}
	if bw.IsFlushed() {
		t.Error("IsFlushed should be false after Drain on empty buffer")
	}
}

// TestBufferedWriter_Reset verifies that Reset() drops buffered bytes
// without committing them. Used by the streaming retry path to discard
// reasoning-only events.
func TestBufferedWriter_Reset(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := newBufferedWriter(rec)
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(200)
	bw.Write([]byte("to-be-dropped"))
	bw.Reset()

	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body after Reset, got %q", rec.Body.String())
	}
	if bw.IsFlushed() {
		t.Error("Reset should not flip IsFlushed")
	}

	// Subsequent writes still buffered, normal path resumes.
	bw.Write([]byte("kept"))
	bw.Flush()
	if rec.Body.String() != "kept" {
		t.Errorf("expected body=kept after Flush, got %q", rec.Body.String())
	}
}

// ---- Progressive-flush regression test ----

// chunkRecorder wraps an httptest.ResponseRecorder and timestamps each
// Write call. The slice of arrival times lets tests assert that the body
// arrived progressively (multiple writes spread over time), not all at
// once at EOF.
type chunkRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	writeTimes []time.Time
	writeSizes []int
}

func newChunkRecorder() *chunkRecorder {
	return &chunkRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writeTimes = append(c.writeTimes, time.Now())
	c.writeSizes = append(c.writeSizes, len(p))
	c.mu.Unlock()
	return c.ResponseRecorder.Write(p)
}

func (c *chunkRecorder) WriteHeader(code int) {
	c.ResponseRecorder.WriteHeader(code)
}

func (c *chunkRecorder) Flush() {
	c.ResponseRecorder.Flush()
}

func (c *chunkRecorder) Times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, len(c.writeTimes))
	copy(out, c.writeTimes)
	return out
}

func (c *chunkRecorder) Sizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(c.writeSizes))
	copy(out, c.writeSizes)
	return out
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_ProgressiveFlush is the
// core regression test for the bug where retry_on_empty=true caused
// streaming responses to be delivered all-at-once at end of stream.
//
// Test setup: downstream emits three SSE events with deliberate 100ms
// gaps. The first event is reasoning-only (treated as empty), the second
// is real content, the third is more content.
//
// Expected behavior with the fix:
//   - The reasoning-only event is BUFFERED but NOT delivered to the client
//     (so the upstream can be transparently retried if the stream ends
//     empty).
//   - The first content event triggers a flush of the buffered bytes to
//     the client (a single burst of reasoning + content), then switches to
//     pass-through mode.
//   - The second content event is delivered progressively (visible as a
//     separate Write call to the recorder, separated in time from the first).
//
// Before the fix, all three events would arrive in a single Write at end
// of stream.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_ProgressiveFlush(t *testing.T) {
	s := newTestStore(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Event 1: reasoning-only.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		// Event 2: real content.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		// Event 3: more content.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))

	// Use a chunkRecorder to observe when each Write arrives.
	cw := newChunkRecorder()
	eng.HandleProxy(cw, req)

	if cw.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", cw.Code)
	}

	// Body must contain the content (and may or may not contain reasoning
	// depending on buffering, but content must be present).
	bodyStr := cw.Body.String()
	if !strings.Contains(bodyStr, `"content":"hello"`) {
		t.Errorf("expected body to contain first content event, got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, `"content":" world"`) {
		t.Errorf("expected body to contain second content event, got %q", bodyStr)
	}

	// Progressive-flush assertion: we must have seen MULTIPLE Write calls
	// with content, spread over time. Specifically, the FIRST content
	// delivery should arrive before the downstream has finished emitting
	// all events (which takes ~200ms).
	times := cw.Times()
	if len(times) < 2 {
		t.Fatalf("expected multiple Write calls for progressive streaming, got %d", len(times))
	}

	// The actual progressive-flush assertion is the timing check below,
	// which compares the time span across multiple Write calls.

	// Compute time of FIRST write vs LAST write.
	firstWrite := times[0]
	lastWrite := times[len(times)-1]
	span := lastWrite.Sub(firstWrite)

	// With the fix, span must be > 50ms (the downstream's inter-event
	// delay was 100ms, so even one burst + pass-through gives > 100ms).
	// Without the fix, span would be < 5ms (everything in one Write).
	if span < 50*time.Millisecond {
		t.Errorf("expected streaming to span > 50ms (progressive flush), got %v across %d Writes", span, len(times))
	}
	t.Logf("streaming took %v across %d Write calls (first non-zero size: %d bytes)", span, len(times), firstNonZero(cw.Sizes()))
}

// firstNonZero returns the index of the first non-zero entry, or -1 if none.
func firstNonZero(sizes []int) int {
	for i, s := range sizes {
		if s > 0 {
			return i
		}
	}
	return -1
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_ReasoningOnlyRetries
// verifies that when the entire stream is reasoning-only (no real content)
// and terminates with [DONE], the gateway retries the upstream request
// and the second attempt's content reaches the client.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_ReasoningOnlyRetries(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		if attempts == 1 {
			// First attempt: reasoning-only stream.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// Terminate without content.
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		} else {
			// Second attempt: real content.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts < 2 {
		t.Errorf("expected at least 2 downstream attempts (retry on reasoning-only empty), got %d", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 after retry, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"content":"recovered"`) {
		t.Errorf("expected recovered content from retry attempt, got %q", w.Body.String())
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_PreservesReasoning is the
// regression test for the user-reported bug: when retry_on_empty=true was
// enabled, reasoning blocks were being STRIPPED from the streaming
// response. With the fix, reasoning tokens must be streamed to the
// client progressively while still triggering retry on reasoning-only
// (terminal [DONE] held) and fully-empty streams.
//
// Test setup: downstream emits a reasoning event followed by a content
// event. Expected behavior with the fix:
//   - The reasoning event is streamed to the client (preserved).
//   - The content event is streamed progressively.
//   - [DONE] is also streamed (since real content was produced).
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_PreservesReasoning(t *testing.T) {
	s := newTestStore(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Reasoning-only event (must be preserved in client output).
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"let me think...\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		// Real content event.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"the answer is 42\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// [DONE] terminal event.
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	bodyStr := w.Body.String()
	// Reasoning block MUST be preserved.
	if !strings.Contains(bodyStr, "reasoning_content") {
		t.Errorf("expected reasoning block to be preserved, got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "let me think") {
		t.Errorf("expected reasoning text to be preserved, got %q", bodyStr)
	}
	// Content must also be present.
	if !strings.Contains(bodyStr, `"content":"the answer is 42"`) {
		t.Errorf("expected content event in body, got %q", bodyStr)
	}
	// [DONE] must be present (real content was produced).
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("expected [DONE] terminal marker in body, got %q", bodyStr)
	}
}

// ---- regression: Anthropic streaming retry-on-empty over multi-format downstream ----

// anthropicSSEdownstream returns an httptest server that emits a per-attempt
// Anthropic SSE body. attempts is incremented on every hit so the test can
// assert a retry happened.
//
// sseBodies is indexed by attempt number (0 = first try, 1 = first retry, ...).
// Each body must be a complete Anthropic SSE stream including the trailing
// blank line; the helper writes the body verbatim with \n line endings (the
// engine's SSE scanner strips \r, so this is compatible with the captured
// .txt files).
func anthropicSSEdownstream(t *testing.T, attempts *int, sseBodies []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := 0
		if attempts != nil {
			*attempts++
			idx = *attempts - 1
		}
		var body string
		if idx < len(sseBodies) {
			body = sseBodies[idx]
		} else if len(sseBodies) > 0 {
			body = sseBodies[len(sseBodies)-1]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Stream with a flush per event so the engine's progressive path is
		// exercised. Trim the trailing whitespace before splitting so we can
		// detect the blank-line-between-events separator.
		body = strings.TrimRight(body, "\r\n")
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprint(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicThinkingOnlyRetries
// is the regression test for the user-reported bug where a multi-format
// downstream ([openai, anthropic], like the user's "Friday" / llama-swap
// instance) returned a thinking-only Anthropic SSE stream and the engine
// never retried.
//
// Root cause: HandleProxy used ApiFormats[0] ("openai") as the response
// format. The streaming handler's on-the-fly detection was gated on the
// seed being empty and passed the accumulated data payload instead of the
// event name to DetectStreamFormat, so the wrong seed was never corrected.
// The OpenAI content classifier naively substring-matches "content", which
// matches Anthropic's message_start ("content":[]) — flipping
// contentProduced=true on the first event and aborting retry.
//
// Fix: use inputFormat when the downstream supports it (so the seed
// matches the actual response shape), and have the on-the-fly detector
// override the seed whenever it recognizes a concrete format.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicThinkingOnlyRetries(t *testing.T) {
	s := newTestStore(t)

	// Attempt 1: thinking-only stream (mirrors not_retried_empty_response_1.txt).
	// Attempt 2: real text_delta content.
	body1 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","content":[],"model":"mock","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking..."}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":13}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	body2 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_y","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"recovered"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var attempts int
	ts := anthropicSSEdownstream(t, &attempts, []string{body1, body2})
	defer ts.Close()
	// Multi-format downstream, like Friday.
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai", "anthropic")
	addOutputModelIDs(t, s, "ds1", "mock-anthropic-model")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"mock-anthropic-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts < 2 {
		t.Fatalf("expected downstream retried at least once (attempt 1 was thinking-only empty), got %d total attempts", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"text":"recovered"`) {
		t.Errorf("expected client body to contain retry's recovered text, got %q", respBody)
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicCompletelyEmptyRetries
// is the regression test for the second captured response
// (not_retried_empty_response_2.txt) — a completely empty Anthropic stream
// (just message_start + message_delta + message_stop, no content blocks).
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicCompletelyEmptyRetries(t *testing.T) {
	s := newTestStore(t)

	body1 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_z","type":"message","role":"assistant","content":[],"model":"mock","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	body2 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_w","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var attempts int
	ts := anthropicSSEdownstream(t, &attempts, []string{body1, body2})
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai", "anthropic")
	addOutputModelIDs(t, s, "ds1", "mock-anthropic-model")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"mock-anthropic-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts < 2 {
		t.Fatalf("expected downstream retried at least once (attempt 1 was completely empty), got %d total attempts", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Errorf("expected client body to contain retry's text, got %q", w.Body.String())
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_AnthropicNonStreaming_NoSpuriousRetry
// is the regression test for the non-streaming side of the same root cause.
// Before the fix, a multi-format [openai, anthropic] downstream returning
// a real-content Anthropic body ({"content":[{"type":"text",...}]}) had
// its downstreamFormat computed as "openai", so IsOpenAIChatEmpty saw no
// "choices" field and reported it as empty — triggering spurious retries
// (and ultimately a 502) on real content.
//
// After the fix, downstreamFormat follows inputFormat when the downstream
// supports it, so IsAnthropicEmpty is the parser used and real content is
// recognized as non-empty. The negative case below asserts no retry; the
// positive case asserts a legitimately-empty Anthropic body still retries.
func TestEngine_HandleProxy_RetryOnEmpty_AnthropicNonStreaming_NoSpuriousRetry(t *testing.T) {
	t.Run("real content not spuriously retried", func(t *testing.T) {
		s := newTestStore(t)
		var attempts int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"real answer"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`)
		}))
		defer ts.Close()
		addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai", "anthropic")
		addOutputModelIDs(t, s, "ds1", "mock-anthropic-model")

		eng := New(s)
		eng.SetRegistry(&mockRegistryImpl{})
		eng.SetRetryOnEmpty(true)

		body := `{"model":"mock-anthropic-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		eng.HandleProxy(w, req)

		if attempts != 1 {
			t.Errorf("expected downstream called once (no spurious retry on real content), got %d", attempts)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"text":"real answer"`) {
			t.Errorf("expected client body to contain real answer, got %q", w.Body.String())
		}
	})

	t.Run("legitimately empty content still retries", func(t *testing.T) {
		s := newTestStore(t)
		var attempts int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if attempts == 1 {
				fmt.Fprint(w, `{"id":"msg_y","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}`)
			} else {
				fmt.Fprint(w, `{"id":"msg_z","type":"message","role":"assistant","content":[{"type":"text","text":"recovered"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`)
			}
		}))
		defer ts.Close()
		addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai", "anthropic")
		addOutputModelIDs(t, s, "ds1", "mock-anthropic-model")

		eng := New(s)
		eng.SetRegistry(&mockRegistryImpl{})
		eng.SetRetryOnEmpty(true)

		body := `{"model":"mock-anthropic-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		eng.HandleProxy(w, req)

		if attempts != 2 {
			t.Errorf("expected downstream retried once (empty Anthropic body), got %d", attempts)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"text":"recovered"`) {
			t.Errorf("expected client body to contain retry's text, got %q", w.Body.String())
		}
	})
}

// TestIsStreamContentLine_AnthropicEmptyStreamEvents pins the unit-level
// classification for every event shape that appears in the user's captured
// thinking-only and empty Anthropic streams. With streamFormat="anthropic"
// (post-fix), none of these events are content, so the engine's
// contentProduced stays false and retry fires.
func TestIsStreamContentLine_AnthropicEmptyStreamEvents(t *testing.T) {
	events := []struct {
		name string
		line string
	}{
		{"message_start", `data: {"type":"message_start","message":{"content":[],"usage":{"output_tokens":0}}}`},
		{"content_block_start_thinking", `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		{"thinking_delta", `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`},
		{"signature_delta", `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`},
		{"content_block_stop", `data: {"type":"content_block_stop","index":0}`},
		{"message_delta", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":13}}`},
		{"message_stop", `data: {"type":"message_stop"}`},
	}
	for _, e := range events {
		t.Run(e.name, func(t *testing.T) {
			if IsStreamContentLine(e.line, "anthropic") {
				t.Errorf("Anthropic event %q should be classified as empty content", e.name)
			}
		})
	}

	// Sanity: real text_delta IS content, confirming the classifier works.
	if !IsStreamContentLine(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, "anthropic") {
		t.Error("Anthropic text_delta must be classified as content")
	}

	// And the terminal events (message_delta, message_stop) ARE recognized
	// as terminal under the anthropic format, so the engine holds them
	// until end-of-stream and can suppress them when retrying.
	terminalEvents := []string{
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	}
	for _, d := range terminalEvents {
		if !isTerminalEvent(d, "anthropic") {
			t.Errorf("Anthropic terminal event must be recognized, got %q", d)
		}
	}
}

// TestIsStreamContentLine_OpenAIEmptyArrayContent pins the IsStreamContentLine
// defensive hardening: an OpenAI chunk carrying `"content":[]` (an
// empty-array content, sometimes emitted as a placeholder) must NOT be
// classified as content. Before this fix, only "content":null and
// "content":"" were excluded, so an empty-array content could still pass
// the substring gate and incorrectly flip contentProduced=true.
func TestIsStreamContentLine_OpenAIEmptyArrayContent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "empty array content not content", line: `data: {"choices":[{"delta":{"content":[]}}]}`, want: false},
		{name: "null content not content", line: `data: {"choices":[{"delta":{"content":null}}]}`, want: false},
		{name: "empty string content not content", line: `data: {"choices":[{"delta":{"content":""}}]}`, want: false},
		{name: "non-empty content IS content", line: `data: {"choices":[{"delta":{"content":"hi"}}]}`, want: true},
		{name: "array with one empty string IS content (defensive: only literal [] excluded)", line: `data: {"choices":[{"delta":{"content":[""]}}]}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStreamContentLine(tt.line, "openai"); got != tt.want {
				t.Errorf("IsStreamContentLine(%q, openai) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicOnOpenAISeededDownstream
// exercises the on-the-fly detector branch (Fix B) independently of Fix A:
// a downstream whose declared api_formats=[openai] returns Anthropic SSE.
// Fix A cannot help here because inputFormat="anthropic" is NOT in
// api_formats, so the seed stays "openai". Without Fix B the OpenAI
// substring classifier would match "content" in message_start and abort
// retry; Fix B's detection must recognize the Anthropic event names and
// override the seed so the Anthropic classifier runs.
func TestEngine_HandleProxy_RetryOnEmpty_Streaming_AnthropicOnOpenAISeededDownstream(t *testing.T) {
	s := newTestStore(t)

	body1 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_a","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ponder"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	body2 := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_b","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"recovered"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var attempts int
	ts := anthropicSSEdownstream(t, &attempts, []string{body1, body2})
	defer ts.Close()
	// OpenAI-only declaration: Fix A's inputFormat branch does NOT fire,
	// so the seed is "openai" and only Fix B can save us.
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai")
	addOutputModelIDs(t, s, "ds1", "mock-anthropic-model")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"mock-anthropic-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts < 2 {
		t.Fatalf("expected downstream retried at least once (Fix B should override 'openai' seed), got %d total attempts", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"recovered"`) {
		t.Errorf("expected client body to contain retry's text, got %q", w.Body.String())
	}
}

// ---- regression: chained instances both with retry_on_empty ----

// engineGatewayServer wraps an Engine in an httptest.Server that routes every
// request to eng.HandleProxy, so it can be used as the downstream of another
// engine (a chained-gateway scenario).
func engineGatewayServer(t *testing.T, eng *Engine) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eng.HandleProxy(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEngine_RetryOnEmpty_Streaming_SingleCleanTerminal is the regression
// guard for the trailing-orphan bug: with retry_on_empty on and a normal
// non-empty Anthropic stream, the client must receive exactly one
// `event: message_stop` and one `data: {"type":"message_stop"}`. Before the
// passthrough branch was made event-buffered, the terminal's data line was
// re-emitted after the real message_stop had already been flushed, appending
// a duplicate.
func TestEngine_RetryOnEmpty_Streaming_SingleCleanTerminal(t *testing.T) {
	s := newTestStore(t)

	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var attempts int
	ts := anthropicSSEdownstream(t, &attempts, []string{body})
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "anthropic")
	addOutputModelIDs(t, s, "ds1", "mock-model")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(true)

	reqBody := `{"model":"mock-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(reqBody)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if attempts != 1 {
		t.Fatalf("expected exactly 1 downstream call (non-empty, no retry), got %d", attempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if n := strings.Count(out, `event: message_stop`); n != 1 {
		t.Errorf("expected exactly one 'event: message_stop', got %d (body=%q)", n, out)
	}
	if n := strings.Count(out, `data: {"type":"message_stop"}`); n != 1 {
		t.Errorf("expected exactly one 'data: {\"type\":\"message_stop\"}', got %d (body=%q)", n, out)
	}
	if !strings.Contains(out, `"text":"hello"`) {
		t.Errorf("expected content in body, got %q", out)
	}
}

// TestEngine_RetryOnEmpty_ChainedInstancesBothOn is the regression test for
// the user-reported bug: two chained gateway instances, both with
// retry_on_empty enabled, serving a non-empty Anthropic stream. The outer
// gateway's client must receive a well-formed stream ending in exactly one
// `event: message_stop` immediately followed by its single
// `data: {"type":"message_stop"}` — no orphan terminal appended by either
// tier. Before the fix, each tier re-emitted its held terminal's data line
// after the real message_stop, and the outer tier re-read the inner tier's
// orphan as a fresh event and compounded it, so the Anthropic SDK reported
// "stream ended before message_stop".
func TestEngine_RetryOnEmpty_ChainedInstancesBothOn(t *testing.T) {
	// Real Anthropic-format downstream.
	realBody := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_r","type":"message","role":"assistant","content":[],"model":"mock","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"inner answer"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var realAttempts int
	realTS := anthropicSSEdownstream(t, &realAttempts, []string{realBody})
	defer realTS.Close()

	// Inner gateway: downstream is the real Anthropic server.
	innerStore := newTestStore(t)
	addDownstream(t, innerStore, "ds-inner", "inner", realTS.URL, "key-inner", "anthropic")
	addOutputModelIDs(t, innerStore, "ds-inner", "mock-model")
	innerEng := New(innerStore)
	innerEng.SetRegistry(&mockRegistryImpl{})
	innerEng.SetRetryOnEmpty(true) // both tiers on
	innerSrv := engineGatewayServer(t, innerEng)

	// Outer gateway: downstream is the inner gateway.
	outerStore := newTestStore(t)
	addDownstream(t, outerStore, "ds-outer", "outer", innerSrv.URL, "key-outer", "anthropic")
	addOutputModelIDs(t, outerStore, "ds-outer", "mock-model")
	outerEng := New(outerStore)
	outerEng.SetRegistry(&mockRegistryImpl{})
	outerEng.SetRetryOnEmpty(true) // both tiers on

	reqBody := `{"model":"mock-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(reqBody)))
	w := httptest.NewRecorder()
	outerEng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"text":"inner answer"`) {
		t.Fatalf("expected inner content in body, got %q", out)
	}
	if n := strings.Count(out, `event: message_stop`); n != 1 {
		t.Errorf("expected exactly one 'event: message_stop' from the outer gateway, got %d (body=%q)", n, out)
	}
	if n := strings.Count(out, `data: {"type":"message_stop"}`); n != 1 {
		t.Errorf("expected exactly one 'data: {\"type\":\"message_stop\"}' from the outer gateway, got %d (body=%q)", n, out)
	}
	// The message_stop event must be intact and terminal: the event line
	// directly precedes its data line, and nothing follows after the event's
	// terminating blank line.
	idx := strings.Index(out, `event: message_stop`)
	if idx == -1 {
		t.Fatalf("missing 'event: message_stop' in body: %q", out)
	}
	tail := out[idx:]
	want := `event: message_stop
data: {"type":"message_stop"}

`
	if !strings.HasPrefix(tail, want) {
		t.Errorf("message_stop event not well-formed / not at end of stream.\n tail=%q\n want prefix=%q", tail, want)
	}
}

// TestEngine_HandleProxy_Passthrough_UsageScraped is the regression test for
// the user-reported bug where the Logs tab showed cache hit rate as N/A even
// though the Inspect view (raw + parsed) clearly carried cache_read_input_tokens.
//
// Trigger: an Anthropic-format SSE stream served by a downstream whose
// api_formats already includes anthropic (and no rule contributes a stream
// transformer) — the engine takes the *passthrough* streaming path, which
// historically forwarded each SSE event without scraping usage, so entry.Usage
// stayed nil and the web UI rendered it as N/A.
//
// The transform path always called scrapeUsage per event; the passthrough path
// now does the same, so this test must see the accumulated usage and a
// non-nil cache hit rate.
func TestEngine_HandleProxy_Passthrough_UsageScraped(t *testing.T) {
	s := newTestStore(t)

	// Multi-format downstream like llama-swap: client speaks anthropic so the
	// engine must NOT auto-translate and must take the passthrough path.
	var attempts int
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_u1","type":"message","role":"assistant","content":[],"model":"mock-model","stop_reason":null,"stop_sequence":null,"usage":{"cache_read_input_tokens":42691,"input_tokens":366,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":260}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	ts := anthropicSSEdownstream(t, &attempts, []string{body})
	defer ts.Close()

	// Multi-format downstream (openai, anthropic); anthropic request => no
	// auto-translation => hasTransformers == false => passthrough branch.
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1", "openai", "anthropic")
	addOutputModelIDs(t, s, "ds1", "mock-model")

	eng := New(s)
	defer eng.Stop()
	eng.SetRegistry(&mockRegistryImpl{})
	// Enable capture so scrapeUsage is active (mirrors capture_payloads: true).
	eng.SetPayloadStore(inspect.New(10))

	reqBody := `{"model":"mock-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if attempts != 1 {
		t.Fatalf("expected exactly one downstream attempt, got %d", attempts)
	}

	entries := eng.GetLogger().RecentEntries(1)
	if len(entries) != 1 {
		t.Fatalf("expected one log entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Usage == nil {
		t.Fatalf("expected entry.Usage to be populated on a passthrough Anthropic SSE stream; got nil (this is the N/A bug)")
	}
	if entry.Usage.InputTokens == nil || *entry.Usage.InputTokens != 366 {
		t.Errorf("expected input_tokens=366, got %v", entry.Usage.InputTokens)
	}
	if entry.Usage.CacheReadTokens == nil || *entry.Usage.CacheReadTokens != 42691 {
		t.Errorf("expected cache_read_input_tokens=42691, got %v", entry.Usage.CacheReadTokens)
	}
	if entry.Usage.OutputTokens == nil || *entry.Usage.OutputTokens != 260 {
		t.Errorf("expected output_tokens=260, got %v", entry.Usage.OutputTokens)
	}
	rate, ok := entry.Usage.CacheHitRate()
	if !ok || rate == nil {
		t.Fatalf("expected CacheHitRate() to be present (ok=true), got ok=%v rate=%v", ok, rate)
	}
	// Expected rate = 42691 / (42691 + 366) ≈ 0.99151… → rounded to 4 decimals: 0.9915.
	const want = 0.9915
	if diff := *rate - want; diff < -1e-6 || diff > 1e-6 {
		t.Errorf("expected cache hit rate ≈ %.4f, got %f", want, *rate)
	}
}