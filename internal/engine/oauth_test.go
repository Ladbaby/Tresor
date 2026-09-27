package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"tresor/internal/config"
)

// fakeTokenManager implements TokenResolver with a fixed token and optional
// extra headers, so we can assert the engine injects it into the Bearer
// header of forwarded requests.
type fakeTokenManager struct {
	token   string
	extra   map[string]string
	err     error
	calls   int
	callsMu sync.Mutex
}

func (f *fakeTokenManager) ResolveValidToken(downstreamID string) (string, map[string]string, error) {
	f.callsMu.Lock()
	f.calls++
	f.callsMu.Unlock()
	return f.token, f.extra, f.err
}

// TestEngine_OAuthTokenInjectedAsBearer verifies that when a downstream is
// configured with auth_method=oauth, the engine replaces the (empty) API key
// with the resolved OAuth token and forwards it as the Bearer credential.
func TestEngine_OAuthTokenInjectedAsBearer(t *testing.T) {
	s := newTestStore(t)

	var gotAuth string
	var gotXAPIKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "", "openai") // empty API key
	if err := s.SetDownstreamAuth("ds1", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	fm := &fakeTokenManager{token: "OAUTH-TOKEN-XYZ", extra: map[string]string{"X-Forwarded-User": "u1"}}
	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetTokenManager(fm)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer OAUTH-TOKEN-XYZ" {
		t.Errorf("expected Bearer OAUTH-TOKEN-XYZ, got %q", gotAuth)
	}
	if gotXAPIKey != "" {
		t.Errorf("expected empty x-api-key, got %q", gotXAPIKey)
	}
}

// TestEngine_OAuthNotConnectedReturns401 verifies that an OAuth downstream
// with no usable token returns a 401 without forwarding to the downstream.
func TestEngine_OAuthNotConnectedReturns401(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(200)
		io.WriteString(w, `{}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "", "openai")
	if err := s.SetDownstreamAuth("ds1", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	fm := &fakeTokenManager{err: errors.New("provider not connected")}
	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetTokenManager(fm)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d (body=%q)", w.Code, w.Body.String())
	}
	if attempts != 0 {
		t.Errorf("downstream must NOT be called when not connected, got %d calls", attempts)
	}
	if !strings.Contains(w.Body.String(), "not connected") {
		t.Errorf("expected 'not connected' in body, got %q", w.Body.String())
	}
}

// TestEngine_APIKeyDownstreamUnaffectedByTokenManager verifies that when no
// token manager is configured (the common case), api_key auth is unchanged.
func TestEngine_APIKeyDownstreamUnaffectedByTokenManager(t *testing.T) {
	s := newTestStore(t)

	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "sk-secret", "openai")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s) // no token manager set
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("expected Bearer sk-secret, got %q", gotAuth)
	}
}

// codexTokenManager is a fake TokenResolver whose extra headers carry the
// chatgpt-account-id marker the OAuth manager injects for a real ChatGPT/Codex
// token, so the engine treats the downstream as the Codex backend.
type codexTokenManager struct{ calls int }

func (f *codexTokenManager) ResolveValidToken(string) (string, map[string]string, error) {
	f.calls++
	return "codex-token", map[string]string{
		"chatgpt-account-id": "acct-123",
		"originator":         "pi",
		"User-Agent":         "pi (win32) ",
	}, nil
}

// TestEngine_CodexBackendFingerprint verifies a real ChatGPT/Codex request is
// fingerprinted like the reference client: OpenAI-Beta set, matching
// session-id / x-client-request-id, and a zstd-compressed body (decodes back
// to the original JSON) with a matching content-encoding header.
func TestEngine_CodexBackendFingerprint(t *testing.T) {
	s := newTestStore(t)

	var (
		gotBeta        string
		gotSession     string
		gotClientReqID string
		gotEncoding    string
		gotAccountID   string
		gotBody        []byte
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotSession = r.Header.Get("session-id")
		gotClientReqID = r.Header.Get("x-client-request-id")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "", "openai")
	if err := s.SetDownstreamAuth("ds1", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	addOutputModelIDs(t, s, "ds1", "gpt-5-codex")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetTokenManager(&codexTokenManager{})

	body := `{"model":"gpt-5-codex","stream":true,"input":[{"role":"user","content":"a somewhat long prompt that is repeated enough to be worth compressing repeated repeated repeated repeated repeated"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if gotBeta != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q, want responses=experimental", gotBeta)
	}
	if gotSession == "" || gotSession != gotClientReqID {
		t.Errorf("session-id=%q x-client-request-id=%q — want non-empty and equal", gotSession, gotClientReqID)
	}
	if gotAccountID != "acct-123" {
		t.Errorf("chatgpt-account-id = %q, want acct-123", gotAccountID)
	}
	if gotEncoding != "zstd" {
		t.Fatalf("content-encoding = %q, want zstd", gotEncoding)
	}
	// The compressed body must decode back to a valid JSON object.
	zr, err := zstd.NewReader(bytes.NewReader(gotBody))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("zstd decompress: %v", err)
	}
	// The decoded body must preserve the original fields and carry the
	// reference client's fingerprint fields.
	var decoded map[string]interface{}
	if err := json.Unmarshal(decompressed, &decoded); err != nil {
		t.Fatalf("decompressed body is not JSON: %v (body=%q)", err, string(decompressed))
	}
	if got, _ := decoded["model"].(string); got != "gpt-5-codex" {
		t.Errorf("decoded model = %v, want gpt-5-codex", decoded["model"])
	}
	if got, _ := decoded["store"].(bool); got != false {
		t.Errorf("decoded store = %v, want false", decoded["store"])
	}
	if inc, ok := decoded["include"].([]interface{}); !ok || len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("decoded include = %v, want [reasoning.encrypted_content]", decoded["include"])
	}
	if got, _ := decoded["prompt_cache_key"].(string); got != gotSession {
		t.Errorf("prompt_cache_key = %q, want it to match session-id %q", got, gotSession)
	}
}

// TestEngine_CodexNonStreamNotCompressed verifies the reference-client parity
// scope: zstd compression is applied only to stream requests, so a non-stream
// ChatGPT/Codex request keeps a plain JSON body (no content-encoding), while
// still carrying the fingerprint headers.
func TestEngine_CodexNonStreamNotCompressed(t *testing.T) {
	s := newTestStore(t)

	var (
		gotEncoding string
		gotBeta     string
		gotBody     []byte
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "", "openai")
	if err := s.SetDownstreamAuth("ds1", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetTokenManager(&codexTokenManager{})

	body := `{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if gotEncoding != "" {
		t.Errorf("non-stream request must not be zstd-compressed, got content-encoding %q", gotEncoding)
	}
	// Body is plain JSON (not compressed) but still carries the reference
	// store:false field; include/prompt_cache_key are Responses-only so a
	// Chat-format body is unchanged apart from that.
	var decoded map[string]interface{}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("non-stream body should be plain JSON, got: %v (%q)", err, string(gotBody))
	}
	if got, _ := decoded["store"].(bool); got != false {
		t.Errorf("decoded store = %v, want false", decoded["store"])
	}
	if _, has := decoded["prompt_cache_key"]; has {
		t.Errorf("Chat-format (non-Responses) body should not get prompt_cache_key, got %v", decoded["prompt_cache_key"])
	}
	if gotBeta != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q, want responses=experimental", gotBeta)
	}
}

func TestEngine_CodexBackendNoRetryOnEmpty(t *testing.T) {
	s := newTestStore(t)

	var attempts int
	var attemptsMu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptsMu.Lock()
		attempts++
		attemptsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// An empty OpenAI Chat response.
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "", "openai")
	if err := s.SetDownstreamAuth("ds1", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})
	eng.SetTokenManager(&codexTokenManager{})
	eng.SetRetryOnEmpty(true)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	attemptsMu.Lock()
	n := attempts
	attemptsMu.Unlock()
	if n != 1 {
		t.Fatalf("Codex backend must not be replayed: got %d upstream calls, want 1", n)
	}
}

