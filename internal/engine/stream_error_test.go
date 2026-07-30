package engine

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseStreamError covers the in-band SSE error classifier. The critical
// property is conservatism: ordinary content must never be mistaken for an
// error, since a false positive aborts a healthy stream.
func TestParseStreamError(t *testing.T) {
	tests := []struct {
		name       string
		eventType  string
		data       string
		wantErr    bool
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "real-world capture: event error with numeric code",
			eventType:  "error",
			data:       `{"code":500,"message":"Context size has been exceeded.","type":"server_error"}`,
			wantErr:    true,
			wantStatus: 500,
			wantMsg:    "Context size has been exceeded.",
		},
		{
			name:       "anthropic overloaded maps to 529",
			eventType:  "error",
			data:       `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantErr:    true,
			wantStatus: 529,
			wantMsg:    "Overloaded",
		},
		{
			name:       "anthropic rate limit maps to 429",
			eventType:  "error",
			data:       `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantErr:    true,
			wantStatus: http.StatusTooManyRequests,
			wantMsg:    "slow down",
		},
		{
			name:       "openai nested error object with code",
			eventType:  "",
			data:       `{"error":{"message":"Rate limited","code":429}}`,
			wantErr:    true,
			wantStatus: 429,
			wantMsg:    "Rate limited",
		},
		{
			name:       "numeric code as string",
			eventType:  "error",
			data:       `{"code":"503","message":"unavailable"}`,
			wantErr:    true,
			wantStatus: 503,
			wantMsg:    "unavailable",
		},
		{
			name:       "unknown error type falls back to 502",
			eventType:  "error",
			data:       `{"type":"error","error":{"type":"mystery_error"}}`,
			wantErr:    true,
			wantStatus: http.StatusBadGateway,
			wantMsg:    "upstream returned an error event",
		},
		{
			name:       "out-of-range code falls back to 502",
			eventType:  "error",
			data:       `{"code":200,"message":"weird"}`,
			wantErr:    true,
			wantStatus: http.StatusBadGateway,
			wantMsg:    "weird",
		},
		{
			name:       "responses api response.failed",
			eventType:  "response.failed",
			data:       `{"type":"response.failed","response":{"status":"failed"}}`,
			wantErr:    true,
			wantStatus: http.StatusBadGateway,
		},
		// Negative cases — these must NOT be classified as errors.
		{
			name:      "text delta mentioning the word error",
			eventType: "content_block_delta",
			data:      `{"type":"content_block_delta","delta":{"type":"text_delta","text":"an error occurred in your code"}}`,
			wantErr:   false,
		},
		{
			name:      "openai content mentioning error",
			data:      `{"choices":[{"delta":{"content":"error: file not found"}}]}`,
			wantErr:   false,
		},
		{
			name:      "tool call whose name contains error",
			data:      `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"log_error"}}]}}]}`,
			wantErr:   false,
		},
		{
			name:      "DONE sentinel",
			data:      "[DONE]",
			wantErr:   false,
		},
		{
			name:      "empty payload",
			data:      "",
			wantErr:   false,
		},
		{
			name:      "non-JSON payload",
			eventType: "error",
			data:      "not json at all",
			wantErr:   false,
		},
		{
			name:      "message_stop",
			eventType: "message_stop",
			data:      `{"type":"message_stop"}`,
			wantErr:   false,
		},
		{
			name:      "null error field is not an error",
			data:      `{"choices":[{"delta":{"content":"hi"}}],"error":null}`,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, isErr := ParseStreamError(tt.eventType, []byte(tt.data))
			if isErr != tt.wantErr {
				t.Fatalf("ParseStreamError(%q, %q) isErr = %v, want %v", tt.eventType, tt.data, isErr, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tt.wantStatus)
			}
			if tt.wantMsg != "" && got.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", got.Message, tt.wantMsg)
			}
		})
	}
}

// newErrorStreamDownstream returns a test server that writes the given raw SSE
// body as text/event-stream with HTTP 200, and a pointer to its hit counter.
func newErrorStreamDownstream(t *testing.T, sseBody string) (*httptest.Server, *int) {
	t.Helper()
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &attempts
}

// proxyStream drives HandleProxy against a downstream and returns the recorder.
func proxyStream(t *testing.T, ts *httptest.Server, retryOnEmpty bool) *httptest.ResponseRecorder {
	t.Helper()
	s := newTestStore(t)
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetRetryOnEmpty(retryOnEmpty)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)
	return w
}

// realWorldErrorCapture reproduces http500-treated-as-200.txt: a long run of
// SSE keep-alive comments followed by a fatal error event, all under HTTP 200.
func realWorldErrorCapture() string {
	var b strings.Builder
	for i := 0; i < 23; i++ {
		b.WriteString(":\n\n")
	}
	b.WriteString("event: error\n")
	b.WriteString(`data: {"code":500,"message":"Context size has been exceeded.","type":"server_error"}`)
	b.WriteString("\n\n")
	return b.String()
}

// TestHandleProxy_StreamErrorAfterKeepAlives is the regression test for the
// reported bug: the downstream returns HTTP 200 with only keep-alives and then
// an error event, and the gateway must surface the provider's real status
// rather than a misleading 200.
func TestHandleProxy_StreamErrorAfterKeepAlives(t *testing.T) {
	for _, retryOnEmpty := range []bool{false, true} {
		name := "retry_on_empty=false"
		if retryOnEmpty {
			name = "retry_on_empty=true"
		}
		t.Run(name, func(t *testing.T) {
			ts, attempts := newErrorStreamDownstream(t, realWorldErrorCapture())
			w := proxyStream(t, ts, retryOnEmpty)

			if w.Code != http.StatusInternalServerError {
				t.Errorf("expected status 500 from the provider's error payload, got %d (body %q)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Context size has been exceeded") {
				t.Errorf("expected provider message in body, got %q", w.Body.String())
			}
			// An explicit error is a definitive answer — it must not be retried.
			if *attempts != 1 {
				t.Errorf("expected exactly 1 downstream call (no retry on explicit error), got %d", *attempts)
			}
		})
	}
}

// TestHandleProxy_StreamErrorStatusMapping verifies the provider's status code
// is passed through for the shapes different providers emit.
func TestHandleProxy_StreamErrorStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		sse        string
		wantStatus int
	}{
		{
			name:       "anthropic overloaded",
			sse:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
			wantStatus: 529,
		},
		{
			name:       "openai nested code 429",
			sse:        "data: {\"error\":{\"message\":\"Rate limited\",\"code\":429}}\n\n",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "no usable code falls back to 502",
			sse:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"mystery\"}}\n\n",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := newErrorStreamDownstream(t, tt.sse)
			w := proxyStream(t, ts, false)
			if w.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d (body %q)", tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleProxy_StreamErrorAfterContent verifies that when content has
// already been streamed the status necessarily stays 200 (it cannot be
// recalled), but the error event still reaches the client so its SDK can
// surface the failure.
func TestHandleProxy_StreamErrorAfterContent(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"}}]}\n\n" +
		"event: error\ndata: {\"code\":500,\"message\":\"Context size has been exceeded.\"}\n\n"

	ts, attempts := newErrorStreamDownstream(t, sse)
	w := proxyStream(t, ts, false)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (already committed once content streamed), got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "partial answer") {
		t.Errorf("expected streamed content to be preserved, got %q", body)
	}
	if !strings.Contains(body, "Context size has been exceeded") {
		t.Errorf("expected error event to be passed through to the client, got %q", body)
	}
	if *attempts != 1 {
		t.Errorf("expected exactly 1 downstream call, got %d", *attempts)
	}
}

// TestHandleProxy_KeepAlivesThenContent verifies the keep-alive hold does not
// swallow keep-alives on a healthy stream: they must still be delivered, along
// with the content, under a 200.
func TestHandleProxy_KeepAlivesThenContent(t *testing.T) {
	sse := ":\n\n:\n\n" + "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" + "data: [DONE]\n\n"

	ts, attempts := newErrorStreamDownstream(t, sse)
	w := proxyStream(t, ts, false)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"hello"`) {
		t.Errorf("expected content in body, got %q", body)
	}
	if !strings.Contains(body, ":\n") {
		t.Errorf("expected held keep-alive comments to be delivered, got %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("expected [DONE] terminal in body, got %q", body)
	}
	if *attempts != 1 {
		t.Errorf("expected exactly 1 downstream call, got %d", *attempts)
	}
}

// TestHandleProxy_StreamErrorSuppressedFromBody verifies the raw error event is
// not also echoed into the response body when the gateway converts it into an
// HTTP error, so the client sees one coherent error rather than a 200-shaped
// SSE error plus an error body.
func TestHandleProxy_StreamErrorSuppressedFromBody(t *testing.T) {
	ts, _ := newErrorStreamDownstream(t, realWorldErrorCapture())
	w := proxyStream(t, ts, false)

	if strings.Contains(w.Body.String(), "event: error") {
		t.Errorf("expected raw SSE error event to be suppressed when converted to an HTTP error, got %q", w.Body.String())
	}
}

// TestHandleProxy_StreamErrorWithTransformPipeline verifies detection also works
// on the transform path, where a rule contributes a stream transformer. The
// error is inspected before transformers run, so a format translator cannot
// mangle it into something unrecognisable.
func TestHandleProxy_StreamErrorWithTransformPipeline(t *testing.T) {
	ts, attempts := newErrorStreamDownstream(t, realWorldErrorCapture())

	s := newTestStore(t)
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	addRule(t, s, "r1", "pass", "/v1/chat/completions", "", "ds1",
		`[{"plugin_id":"pass_through"}]`, true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 on the transform path, got %d (body %q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Context size has been exceeded") {
		t.Errorf("expected provider message in body, got %q", w.Body.String())
	}
	if *attempts != 1 {
		t.Errorf("expected exactly 1 downstream call, got %d", *attempts)
	}
}
