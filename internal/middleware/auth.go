package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuthCookie is the name of the cookie used for admin API authentication.
// Tokens are stored in this cookie instead of URL query parameters to avoid
// token leakage via access logs, browser history, and referrer headers.
const AuthCookie = "tresor_token"

// CookieMaxAge is the duration of the auth cookie (365 days — "logged in forever").
const CookieMaxAge = 365 * 24 * 3600

// SetAuthCookie sets the auth cookie on the response with secure attributes.
func SetAuthCookie(w http.ResponseWriter, token string) {
	w.Header().Set("Set-Cookie", fmt.Sprintf("%s=%s; Path=/api/; SameSite=Lax; Max-Age=%d", AuthCookie, token, CookieMaxAge))
}

// ClearAuthCookie clears the auth cookie by expiring it immediately.
func ClearAuthCookie(w http.ResponseWriter) {
	w.Header().Set("Set-Cookie", fmt.Sprintf("%s=; Path=/api/; SameSite=Lax; Max-Age=0", AuthCookie))
}

// SecurityHeaders wraps an http.Handler and injects security headers on every
// response, while stripping the Server header.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("Referrer-Policy", "same-origin")
		// Strip Server header to avoid version leakage
		w.Header().Del("Server")
		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware provides authentication for admin API endpoints.
// It supports two authentication methods:
//   - Raw password as Bearer token (backwards compat for CLI)
//   - Session token as Bearer header (for web UI fetch requests)
//   - Session token as cookie (for SSE EventSource, which cannot set custom headers)
type AuthMiddleware struct {
	password string
	token    string // current session token (set on login, cleared on logout)
	tokenMu  sync.RWMutex
	limiter  *rateLimiter
}

// NewAuthMiddleware creates an auth middleware. If password is empty, authentication is disabled.
// A rate limiter is set up: max 5 failed login attempts per 60-second window per IP.
func NewAuthMiddleware(password string) *AuthMiddleware {
	return &AuthMiddleware{
		password: password,
		limiter:  newRateLimiter(5, 60*time.Second),
	}
}

// SetPassword updates the password at runtime.
// Clearing the password also invalidates any active session token.
func (am *AuthMiddleware) SetPassword(password string) {
	am.tokenMu.Lock()
	am.token = "" // invalidate session on password change
	am.tokenMu.Unlock()

	am.password = password
}

// Stop cleans up the rate limiter's background goroutine.
func (am *AuthMiddleware) Stop() {
	if am.limiter != nil {
		am.limiter.Stop()
	}
}

// CheckRateLimit checks if the given IP has exceeded the login rate limit.
// Returns true if the request should be rate-limited (429).
func (am *AuthMiddleware) CheckRateLimit(ip string) bool {
	if am.limiter == nil {
		return false
	}
	return am.limiter.record(ip)
}

// GenerateToken creates a random session token. The token is a 32-byte random
// hex string (64 chars), making it practically unguessable.
func (am *AuthMiddleware) GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("failed to generate random token: " + err.Error())
	}
	am.tokenMu.Lock()
	am.token = hex.EncodeToString(b)
	am.tokenMu.Unlock()
	return am.token
}

// ClearToken invalidates the current session token (used by logout).
func (am *AuthMiddleware) ClearToken() {
	am.tokenMu.Lock()
	am.token = ""
	am.tokenMu.Unlock()
}

// Protect wraps an http.Handler with authentication.
// If no password is configured, it passes through all requests.
func (am *AuthMiddleware) Protect(next http.Handler) http.Handler {
	if am.password == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Re-check at request time: if password has been cleared via SetPassword(""),
		// auth is no longer required.
		if am.password == "" {
			next.ServeHTTP(w, r)
			return
		}

		if am.Authenticate(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// Authenticate checks auth methods: raw password, session token (header or cookie).
// Cookie-based auth is used for SSE EventSource connections (which cannot set
// custom headers). Query parameter auth is not supported — tokens in URLs are
// logged by proxies, load balancers, and browser history.
func (am *AuthMiddleware) Authenticate(r *http.Request) bool {
	// Collect all candidate tokens: Authorization header + cookie
	var tokens []string

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		tokens = append(tokens, strings.TrimPrefix(auth, "Bearer "))
	}

	// Cookie-based auth for SSE EventSource (cookies are sent automatically)
	if cookie, err := r.Cookie(AuthCookie); err == nil && cookie.Value != "" {
		tokens = append(tokens, cookie.Value)
	}

	for _, token := range tokens {
		// Check 1: Raw password match (backwards compat for CLI)
		if subtle.ConstantTimeCompare([]byte(token), []byte(am.password)) == 1 {
			return true
		}

		// Check 2: Session token match
		am.tokenMu.RLock()
		stored := am.token
		am.tokenMu.RUnlock()
		if stored != "" && subtle.ConstantTimeCompare([]byte(token), []byte(stored)) == 1 {
			return true
		}
	}

	return false
}

// Error response helper for the admin API (avoids importing api package).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}

// ExtractClientIP returns the client IP address for rate limiting.
// It strips the port from RemoteAddr and trusts X-Forwarded-For/X-Real-IP
// only when the direct connection is from localhost (reverse proxy scenario).
func ExtractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	// If connected via localhost, trust forwarded headers
	if host == "127.0.0.1" || host == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.Split(xff, ",")[0]
		}
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			return strings.TrimSpace(xrip)
		}
	}
	return host
}
