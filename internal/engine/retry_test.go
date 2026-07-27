package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

	// 200 + empty → retry
	if !eng.shouldRetry(respOK, []byte(`{"choices":[]}`), "openai") {
		t.Error("200 with empty OpenAI body should retry")
	}
	// 200 + non-empty → no retry
	if eng.shouldRetry(respOK, []byte(`{"choices":[{"message":{"content":"hi"}}]}`), "openai") {
		t.Error("200 with non-empty body should NOT retry")
	}
	// 500 + empty → no retry (HTTP errors are the client's responsibility)
	resp500 := &http.Response{StatusCode: 500}
	if eng.shouldRetry(resp500, []byte(`{"choices":[]}`), "openai") {
		t.Error("500 must NOT trigger retry regardless of body")
	}
	// 429 + empty → no retry
	resp429 := &http.Response{StatusCode: 429}
	if eng.shouldRetry(resp429, []byte(`{"choices":[]}`), "openai") {
		t.Error("429 must NOT trigger retry (client handles rate limits)")
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
// verifies that when all retries return empty, the gateway ultimately
// returns 502 to the client.
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
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected status 502 after retries exhausted, got %d", w.Code)
	}
	respBody, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(strings.ToLower(string(respBody)), "empty") {
		t.Errorf("expected error body to mention 'empty', got %q", string(respBody))
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