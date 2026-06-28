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
	mw.ClearToken(token) // logout this session

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

func TestAuthMiddleware_MultipleTokensCoexist(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Two independent logins (e.g. two browsers)
	tokenA := mw.GenerateToken()
	tokenB := mw.GenerateToken()
	if tokenA == tokenB {
		t.Fatalf("tokens must differ: %q == %q", tokenA, tokenB)
	}

	// Token A works via Bearer
	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token A (Bearer): expected 200, got %d", w.Code)
	}

	// Token B works via Bearer — and crucially token A still does
	req = httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token B (Bearer): expected 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token A (Bearer, after B login): expected 200, got %d", w.Code)
	}

	// Token B works via cookie (SSE path)
	req = httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookie, Value: tokenB})
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token B (cookie): expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ClearOneTokenLeavesOther(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	tokenA := mw.GenerateToken()
	tokenB := mw.GenerateToken()

	mw.ClearToken(tokenA)

	// A is gone
	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("token A after clear: expected 401, got %d", w.Code)
	}

	// B survives
	req = httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token B after A clear: expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ClearAllTokensRemovesAll(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	tokenA := mw.GenerateToken()
	tokenB := mw.GenerateToken()

	mw.ClearAllTokens()

	for label, tok := range map[string]string{"A": tokenA, "B": tokenB} {
		req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token %s after ClearAllTokens: expected 401, got %d", label, w.Code)
		}
	}
}

func TestAuthMiddleware_ClearUnknownTokenIsNoop(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	token := mw.GenerateToken()

	// Clearing an unrelated token must not panic or affect token.
	mw.ClearToken("definitely-not-a-real-token")

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token after unknown clear: expected 200, got %d", w.Code)
	}

	// Empty string must also be a no-op (not ClearAllTokens).
	mw.ClearToken("")
	req = httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token after empty clear: expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_SetTokens_RestoresBulk(t *testing.T) {
	mw := NewAuthMiddleware("secret")
	handler := newTestHandler("ok")
	protected := mw.Protect(handler)

	// Simulate restore-from-disk on daemon startup.
	mw.SetTokens([]string{"tok1", "tok2"})

	for _, tok := range []string{"tok1", "tok2"} {
		req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("restored %q: expected 200, got %d", tok, w.Code)
		}
	}
}

func TestAuthMiddleware_OnTokenGenerated_Cleared(t *testing.T) {
	mw := NewAuthMiddleware("secret")

	var generated []string
	var cleared []string
	mw.OnTokenGenerated = func(tok string) error { generated = append(generated, tok); return nil }
	mw.OnTokenCleared = func(tok string) error { cleared = append(cleared, tok); return nil }

	a := mw.GenerateToken()
	b := mw.GenerateToken()
	mw.ClearToken(a)
	mw.ClearAllTokens()

	if len(generated) != 2 || generated[0] != a || generated[1] != b {
		t.Fatalf("OnTokenGenerated: got %v, want [%s %s]", generated, a, b)
	}
	if len(cleared) != 2 || cleared[0] != a || cleared[1] != "" {
		t.Fatalf("OnTokenCleared: got %v, want [%s \"\"]", cleared, a)
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
