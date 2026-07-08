package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/inspect"
)

// newTestRouterWithPayloadStore returns a router wired to a real
// inspect.Store so the inspect endpoint has data to serve. The store is
// pre-populated with one entry under id 42 for tests that need a known id.
func newTestRouterWithPayloadStore(t *testing.T) (*Router, *inspect.Store) {
	t.Helper()
	router := newTestRouter(t)
	store := inspect.New(10)
	router.payloadStore = store
	store.Add(inspect.Entry{
		ID:                 42,
		Path:               "/v1/chat/completions",
		Method:             "POST",
		Model:              "gpt-4o",
		ResolvedModel:      "claude-sonnet-4-20250514",
		DownstreamID:       "anthropic-main",
		Status:             200,
		RequestContentType: "application/json",
		RequestBody:        []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
		ResponseContentType: "application/json",
		ResponseBody:       []byte(`{"content":[{"text":"hi back"}]}`),
	})
	return router, store
}

func TestLogInspect_ReturnsCapturedEntry(t *testing.T) {
	router, _ := newTestRouterWithPayloadStore(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/logs/42/inspect", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		ID            int    `json:"id"`
		Path          string `json:"path"`
		Method        string `json:"method"`
		Model         string `json:"model"`
		ResolvedModel string `json:"resolved_model"`
		DownstreamID  string `json:"downstream_id"`
		Status        int    `json:"status"`
		Request       struct {
			ContentType string `json:"content_type"`
			Body        string `json:"body"`
		} `json:"request"`
		Response struct {
			ContentType string `json:"content_type"`
			Body        string `json:"body"`
		} `json:"response"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 42 {
		t.Fatalf("expected id 42, got %d", got.ID)
	}
	if got.Request.ContentType != "application/json" {
		t.Fatalf("expected request content type, got %q", got.Request.ContentType)
	}
	if !strings.Contains(got.Request.Body, "hello") {
		t.Fatalf("expected request body to contain 'hello', got %q", got.Request.Body)
	}
	if !strings.Contains(got.Response.Body, "hi back") {
		t.Fatalf("expected response body to contain 'hi back', got %q", got.Response.Body)
	}
	if got.Path != "/v1/chat/completions" {
		t.Fatalf("expected path, got %q", got.Path)
	}
	if got.Status != 200 {
		t.Fatalf("expected status 200, got %d", got.Status)
	}
}

func TestLogInspect_NotFoundForUnknownID(t *testing.T) {
	router, _ := newTestRouterWithPayloadStore(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/logs/9999/inspect", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogInspect_NotFoundForInvalidID(t *testing.T) {
	router, _ := newTestRouterWithPayloadStore(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/logs/not-a-number/inspect", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogInspect_NotFoundWhenStoreNil(t *testing.T) {
	// newTestRouter passes a nil payload store. The endpoint must return 404
	// rather than panic.
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/logs/1/inspect", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when store is nil, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogInspect_MethodNotAllowed(t *testing.T) {
	router, _ := newTestRouterWithPayloadStore(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/logs/42/inspect", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d: %s", w.Code, w.Body.String())
	}
}
