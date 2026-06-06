package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestHandler(expectedBody string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(expectedBody))
	})
}

func TestAuthMiddleware_Protect_NoPassword_Passthrough(t *testing.T) {
	mw := NewAuthMiddleware("")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 passthrough with no password, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestAuthMiddleware_Protect_ValidToken_Allows(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestAuthMiddleware_Protect_WrongToken_Rejects(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-password")
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_Protect_MissingHeader_Rejects(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	// No Authorization header set
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with missing header, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_Protect_BadFormat_Rejects(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with bad format, got %d: %s", w.Code, w.Body.String())
	}
}
