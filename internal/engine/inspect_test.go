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

	"tresor/internal/inspect"
	"tresor/internal/store"
)

// mockBodyMutator is a test-only plugin that rewrites the request body and
// the response body so we can verify the inspector captures the *pre-
// transformer* bytes, not the post-transformer bytes. The plugin appends a
// marker field on the request and a marker key on the response.
type mockBodyMutator struct{}

func (m *mockBodyMutator) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	// Mutate the body to a clearly different shape than the original.
	mutated := []byte(`{"model":"MUTATED-BY-PLUGIN","messages":[]}`)
	return req, mutated, nil
}

func (m *mockBodyMutator) TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error) {
	// Append a marker so we can prove the response the inspector sees is
	// what the downstream returned, not what the plugin wrote.
	marker := []byte(`,"_mutated_by_plugin":true`)
	if bytes.HasSuffix(body, []byte("}")) {
		return append(body[:len(body)-1], marker...), nil
	}
	return body, nil
}

func (m *mockBodyMutator) TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error) {
	return chunk, nil
}

// mockRegistryWithMutator is a tiny registry that resolves the mutator
// plugin id used in the inspector capture test.
type mockRegistryWithMutator struct{}

func (m *mockRegistryWithMutator) CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error) {
	if pluginID == "body_mutator" {
		return &mockBodyMutator{}, nil
	}
	return &mockPassThrough{}, nil
}

func (m *mockRegistryWithMutator) ListPlugins() []PluginInfo { return nil }

// TestEngine_HandleProxy_CaptureReflectsPreTransformerBytes verifies the
// inspector sees the *original* client body and the *original* downstream
// response body, even when a registered plugin mutates both. This is the
// core correctness property of the inspector — if it showed the post-
// plugin bytes it would be useless for debugging what the client actually
// sent and what the downstream actually returned.
func TestEngine_HandleProxy_CaptureReflectsPreTransformerBytes(t *testing.T) {
	s := newTestStore(t)

	// Downstream returns a known body. The plugin will append a marker
	// after the engine reads the body, so what the inspector sees must
	// NOT contain the marker.
	const downstreamBody = `{"choices":[{"message":{"content":"hello-from-downstream"}}]}`
	ts := newTestDownstream(t, 200, downstreamBody, nil)
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	// Rule attaches the body-mutating plugin to the request.
	addRule(t, s, "r1", "Mutator", "*", "", "ds1",
		`[{"plugin_id":"body_mutator","config":{}}]`, true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryWithMutator{})
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(true)

	originalBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(originalBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	// The most recent log entry's id is the nextID-1 of the logger, but
	// we don't have to compute it: the engine's logger has just one entry,
	// and the payload store is keyed by the same id. We can find the
	// captured id by walking store ids.
	var capturedID int
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			capturedID = i
			break
		}
	}
	if capturedID == 0 && !hasID0(store2) {
		t.Fatalf("expected at least one captured entry, store len = %d", store2.Len())
	}

	captured, ok := store2.Get(capturedID)
	if !ok {
		t.Fatalf("expected captured entry for id %d", capturedID)
	}

	// Inspector must show the *original* request body, not the plugin's
	// mutated version.
	if string(captured.RequestBody) != originalBody {
		t.Fatalf("expected inspector to show original request body %q, got %q",
			originalBody, string(captured.RequestBody))
	}
	if bytes.Contains(captured.RequestBody, []byte("MUTATED-BY-PLUGIN")) {
		t.Fatalf("inspector request body was post-transformer: %q", captured.RequestBody)
	}

	// Inspector must show the *downstream* response body (downstreamBody),
	// not the plugin's marker-appended version.
	if string(captured.ResponseBody) != downstreamBody {
		t.Fatalf("expected inspector to show original downstream body %q, got %q",
			downstreamBody, string(captured.ResponseBody))
	}
	if bytes.Contains(captured.ResponseBody, []byte("_mutated_by_plugin")) {
		t.Fatalf("inspector response body was post-transformer: %q", captured.ResponseBody)
	}
	if captured.ResponseContentType == "" {
		t.Fatalf("expected response content type to be recorded")
	}
}

// TestEngine_HandleProxy_CaptureDisabledStaysAtZeroCost is a sanity check
// that with capture off, the payload store stays empty. The atomic.Load in
// the hot path is supposed to make the disabled state a single instruction.
func TestEngine_HandleProxy_CaptureDisabledStaysAtZeroCost(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{"ok":true}`, nil)
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryWithMutator{})
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	// capture off
	eng.SetCapturePayloads(false)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if got := store2.Len(); got != 0 {
		t.Fatalf("expected empty store when capture is off, got %d entries", got)
	}
}

// TestEngine_HandleProxy_ToggleCaptureAtRuntime simulates the user flow:
// the daemon starts with capture off, the operator flips the toggle in the
// Web UI (PUT /api/config with capture_payloads=true), and a subsequent
// request must be captured. This is the regression for the case where the
// toggle in the UI updates the config but the engine never actually starts
// capturing.
func TestEngine_HandleProxy_ToggleCaptureAtRuntime(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{"choices":[{"message":{"content":"hello"}}]}`, nil)
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryWithMutator{})
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	// Start with capture OFF, just like a fresh daemon with
	// capture_payloads: false in config.yaml.
	eng.SetCapturePayloads(false)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	// Request 1: capture is off, store stays empty.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)
	if got := store2.Len(); got != 0 {
		t.Fatalf("after request 1 (capture off) expected empty store, got %d entries", got)
	}

	// Operator flips the toggle in the UI; the API handler does
	// r.engine.SetCapturePayloads(true). Mirror that here.
	eng.SetCapturePayloads(true)
	if !eng.CapturePayloads() {
		t.Fatalf("CapturePayloads() returned false after SetCapturePayloads(true)")
	}

	// Request 2: capture is on, store must now contain an entry.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w2 := httptest.NewRecorder()
	eng.HandleProxy(w2, req2)
	if got := store2.Len(); got != 1 {
		t.Fatalf("after request 2 (capture on) expected 1 entry in store, got %d", got)
	}

	// Request 3: another capture-on request, store grows to 2.
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w3 := httptest.NewRecorder()
	eng.HandleProxy(w3, req3)
	if got := store2.Len(); got != 2 {
		t.Fatalf("after request 3 (capture on) expected 2 entries in store, got %d", got)
	}

// Flip back off, verify the next request is not captured.
	eng.SetCapturePayloads(false)
	req4 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w4 := httptest.NewRecorder()
	eng.HandleProxy(w4, req4)
	if got := store2.Len(); got != 2 {
		t.Fatalf("after request 4 (capture off again) expected store to stay at 2, got %d", got)
	}

	// Sanity: the two captured entries must have distinct ids. Before the
	// logger.Record fix, all entries were stored under id 0 because
	// Record took its argument by value, so the engine's caller never
	// saw the assigned id.
	foundIDs := map[int]bool{}
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			foundIDs[i] = true
		}
	}
	if len(foundIDs) != 2 {
		t.Fatalf("expected 2 distinct ids in the store, found %d (ids: %v)", len(foundIDs), foundIDs)
	}
}

// TestEngine_HandleProxy_StreamingToggleCaptureAtRuntime mirrors the
// non-streaming toggle test for the streaming path. The bug was that the
// logger assigned ids by value, so the streaming handler's deferred
// recordAndCapture saw id 0 for every request — every capture clobbered
// the previous one.
func TestEngine_HandleProxy_StreamingToggleCaptureAtRuntime(t *testing.T) {
	s := newTestStore(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if flusher != nil { flusher.Flush() }
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil { flusher.Flush() }
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryWithMutator{})
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(false)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// Capture off
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)
	if got := store2.Len(); got != 0 {
		t.Fatalf("after stream 1 (capture off) expected empty store, got %d", got)
	}

	// Toggle on
	eng.SetCapturePayloads(true)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w2 := httptest.NewRecorder()
	eng.HandleProxy(w2, req2)
	if got := store2.Len(); got != 1 {
		t.Fatalf("after stream 2 (capture on) expected 1 entry, got %d", got)
	}

	// Second capture-on request — must produce a SECOND entry, not clobber the first
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w3 := httptest.NewRecorder()
	eng.HandleProxy(w3, req3)
	if got := store2.Len(); got != 2 {
		t.Fatalf("after stream 3 (capture on) expected 2 entries, got %d — id assignment bug?", got)
	}

	// Verify the two stored entries have distinct ids.
	foundIDs := map[int]bool{}
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			foundIDs[i] = true
		}
	}
	if len(foundIDs) != 2 {
		t.Fatalf("expected 2 distinct ids in store, found %d (ids: %v)", len(foundIDs), foundIDs)
	}
}

// TestEngine_HandleProxy_StreamingCaptureTeesRawBytes verifies the
// streaming path tees raw SSE bytes into the inspector store. The captured
// bytes must contain the literal "data: " framing the downstream sent and
// must NOT be a post-transformer mutation.
func TestEngine_HandleProxy_StreamingCaptureTeesRawBytes(t *testing.T) {
	s := newTestStore(t)

	// Echo-style SSE server: every chat completion response is a stream.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		if flusher != nil { flusher.Flush() }
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		if flusher != nil { flusher.Flush() }
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil { flusher.Flush() }
	}))
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryWithMutator{})
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(true)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	// The streaming handler records the entry when the stream completes;
	// the buffered response writer blocks the test until the client (the
	// recorder) has consumed the whole body, so by here the capture has
	// happened.
	if got := store2.Len(); got != 1 {
		t.Fatalf("expected 1 captured entry, got %d", got)
	}
	// Find the captured entry (its id is the logger's nextID-1, but we
	// don't need to compute it; just iterate to find the one entry).
	var captured *inspect.Entry
	for i := 0; i < 100; i++ {
		if e, ok := store2.Get(i); ok {
			captured = e
			break
		}
	}
	if captured == nil {
		t.Fatalf("captured entry not found")
	}
	// The captured response body must be the raw SSE the downstream sent
	// (verbatim, with the `data: ` framing). It must not be empty, must
	// contain "data: ", and must contain "[DONE]".
	got := string(captured.ResponseBody)
	if got == "" {
		t.Fatalf("captured SSE body is empty")
	}
	if !strings.Contains(got, "data: ") {
		t.Fatalf("expected captured SSE to contain 'data: ' lines, got %q", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("expected captured SSE to contain [DONE] marker, got %q", got)
	}
	if captured.ResponseContentType != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", captured.ResponseContentType)
	}
}


// hasID0 is a small helper because the previous check used 0 as "not found"
// and a real id of 0 would be ambiguous. Reserved for future-proofing.
func hasID0(s *inspect.Store) bool {
	_, ok := s.Get(0)
	return ok
}

// TestEngine_HandleProxy_DownstreamNameIsCaptured verifies that the human-
// readable downstream name propagates into the inspect entry so the UI can
// render "Anthropic Production" rather than "anthropic-prod" in the
// inspector header.
func TestEngine_HandleProxy_DownstreamNameIsCaptured(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil)
	defer ts.Close()
	// Distinct id and name. The name is what the inspector header should
	// surface — this is the regression case that motivated the field.
	addDownstream(t, s, "anthropic-prod", "Anthropic Production", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "anthropic-prod", "gpt-4o")

	eng := New(s)
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}

	var capturedID int
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			capturedID = i
			break
		}
	}
	captured, ok := store2.Get(capturedID)
	if !ok {
		t.Fatalf("no captured entry found")
	}
	if captured.DownstreamID != "anthropic-prod" {
		t.Fatalf("expected downstream id 'anthropic-prod', got %q", captured.DownstreamID)
	}
	if captured.DownstreamName != "Anthropic Production" {
		t.Fatalf("expected downstream name 'Anthropic Production', got %q", captured.DownstreamName)
	}
}

// TestEngine_HandleProxy_ClientIPIsCaptured verifies that the client's IP
// (port-stripped) is captured in the inspect entry so the inspector
// header can show who hit the gateway. The engine pulls from
// r.RemoteAddr directly — no X-Forwarded-For trust.
func TestEngine_HandleProxy_ClientIPIsCaptured(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil)
	defer ts.Close()
	addDownstream(t, s, "anthropic-prod", "Anthropic Production", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "anthropic-prod", "gpt-4o")

	eng := New(s)
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)))
	// httptest.NewRequest sets RemoteAddr to "192.0.2.1:1234" by default
	// (TEST-NET-1 RFC 5737). After port-stripping the inspector should
	// see just "192.0.2.1".
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}

	var capturedID int
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			capturedID = i
			break
		}
	}
	captured, ok := store2.Get(capturedID)
	if !ok {
		t.Fatalf("no captured entry found")
	}
	if captured.ClientIP != "192.0.2.1" {
		t.Fatalf("expected client ip '192.0.2.1' (port-stripped), got %q", captured.ClientIP)
	}
}

// TestEngine_HandleProxy_ClientIP_IPv6 covers the IPv6 path: an IPv6
// address arrives as "[::1]:1234" via RemoteAddr and must be normalised
// to "::1" by the engine's clientIPFromAddr helper.
func TestEngine_HandleProxy_ClientIP_IPv6(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil)
	defer ts.Close()
	addDownstream(t, s, "anthropic-prod", "Anthropic Production", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "anthropic-prod", "gpt-4o")

	eng := New(s)
	store2 := inspect.New(10)
	eng.SetPayloadStore(store2)
	eng.SetCapturePayloads(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "[2001:db8::1]:4242" // RFC 3849 documentation IPv6
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}

	var capturedID int
	for i := 0; i < 100; i++ {
		if _, ok := store2.Get(i); ok {
			capturedID = i
			break
		}
	}
	captured, ok := store2.Get(capturedID)
	if !ok {
		t.Fatalf("no captured entry found")
	}
	if captured.ClientIP != "2001:db8::1" {
		t.Fatalf("expected client ip '2001:db8::1', got %q", captured.ClientIP)
	}
}

// guard against unused-import warnings if the file shrinks.
var _ = store.Rule{}
var _ = json.Marshal
