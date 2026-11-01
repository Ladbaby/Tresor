package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tresor/internal/config"
	"tresor/internal/oauth"
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

func TestEngine_GenericForwardingDoesNotAddCodexFingerprint(t *testing.T) {
	s := newTestStore(t)
	const body = `{"model":"generic-model","stream":true,"input":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("generic body changed: %s", got)
		}
		for _, header := range []string{"OpenAI-Beta", "session-id", "x-client-request-id", "Content-Encoding", "chatgpt-account-id"} {
			if value := r.Header.Get(header); value != "" {
				t.Errorf("generic request leaked %s=%s", header, value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()
	addDownstream(t, s, "generic-ds", "generic-ds", server.URL, "test-key", "openai")
	addOutputModelIDs(t, s, "generic-ds", "generic-model")
	eng := New(s)
	defer eng.Stop()
	eng.SetRegistry(&mockRegistryImpl{})
	w := httptest.NewRecorder()
	eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestEngine_OAuthPreparationErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		manager TokenResolver
		status  int
		message string
	}{
		{"missing manager", nil, 401, "no OAuth support"},
		{"transient refresh", &fakeTokenManager{err: fmt.Errorf("refresh: %w", oauth.ErrTransientRefresh)}, 503, "please retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			addDownstream(t, s, "oauth-ds", "oauth-ds", "http://127.0.0.1:1", "", "openai")
			if err := s.SetDownstreamAuth("oauth-ds", &config.DownstreamAuthCfg{Type: "oauth"}); err != nil {
				t.Fatal(err)
			}
			addOutputModelIDs(t, s, "oauth-ds", "oauth-model")
			eng := New(s)
			defer eng.Stop()
			eng.SetRegistry(&mockRegistryImpl{})
			eng.SetTokenManager(tc.manager)
			w := httptest.NewRecorder()
			eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"oauth-model"}`)))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			entries := eng.GetLogger().RecentEntries(10)
			if len(entries) != 1 || entries[0].Status != tc.status {
				t.Fatalf("logs: %+v", entries)
			}
		})
	}
}

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
