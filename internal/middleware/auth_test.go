package middleware

import (
	"fmt"
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

func TestAuthMiddleware_SessionToken_BearerHeader(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Generate a session token (simulates login)
	token := mw.GenerateToken()

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid session token, got %d", w.Code)
	}
}

func TestAuthMiddleware_SessionToken_Cookie(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Generate a session token (simulates login)
	token := mw.GenerateToken()

	req := httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookie, Value: token})
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid session token cookie, got %d", w.Code)
	}
}

func TestAuthMiddleware_SessionToken_Invalid_Rejects(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Generate a token, then try with a different one
	mw.GenerateToken()

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer wrong_token_value")
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with invalid session token, got %d", w.Code)
	}
}

func TestAuthMiddleware_ClearToken_InvalidatesSession(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	token := mw.GenerateToken()
	mw.ClearToken() // logout

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after token cleared, got %d", w.Code)
	}
}

func TestAuthMiddleware_SetPassword_ClearsToken(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	token := mw.GenerateToken()

	// Change password
	mw.SetPassword("new-password")

	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Old token should no longer work
	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after password change, got %d", w.Code)
	}
}

func TestExtractClientIP_RemoteAddrNormalization(t *testing.T) {
	// Non-localhost: strip port, ignore forwarded headers
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	if got := ExtractClientIP(req); got != "10.0.0.1" {
		t.Errorf("non-localhost: got %q, want %q", got, "10.0.0.1")
	}

	// Localhost with X-Forwarded-For: use first IP
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.1.2.3")
	if got := ExtractClientIP(req); got != "203.0.113.50" {
		t.Errorf("localhost XFF: got %q, want %q", got, "203.0.113.50")
	}

	// Localhost with X-Real-IP
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Real-IP", "203.0.113.50")
	if got := ExtractClientIP(req); got != "203.0.113.50" {
		t.Errorf("localhost XRI: got %q, want %q", got, "203.0.113.50")
	}

	// IPv6 localhost
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::1]:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	if got := ExtractClientIP(req); got != "203.0.113.50" {
		t.Errorf("ipv6 localhost XFF: got %q, want %q", got, "203.0.113.50")
	}

	// No forwarded headers, non-localhost
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.100:54321"
	if got := ExtractClientIP(req); got != "192.168.1.100" {
		t.Errorf("no proxy: got %q, want %q", got, "192.168.1.100")
	}
}

func TestRateLimit_SameHostDifferentPorts(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	// Rate limiter: 5 attempts per 60s. 6th should be blocked.
	// Use same host with different ports to simulate new TCP connections.
	for i := 1; i <= 6; i++ {
		key := fmt.Sprintf("192.168.1.100:%d", 50000+i)
		// Simulate what the router does: normalize the key before passing to limiter
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = key
		ip := ExtractClientIP(req)
		blocked := mw.CheckRateLimit(ip)
		if i <= 5 {
			if blocked {
				t.Errorf("attempt %d: should not be blocked yet", i)
			}
		} else {
			if !blocked {
				t.Errorf("attempt %d: should be blocked (same host, different ports)", i)
			}
		}
	}
}
