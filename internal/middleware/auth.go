package middleware

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// SecurityHeaders wraps an http.Handler and injects security headers on every
// response, while stripping the Server header.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'")
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
// It supports three authentication methods:
//   - Raw password as Bearer token (backwards compat for CLI)
//   - JWT token as Bearer header (for web UI)
//   - JWT token as query parameter (for SSE EventSource)
type AuthMiddleware struct {
	password string
	secret   []byte
	limiter  *rateLimiter
}

// NewAuthMiddleware creates an auth middleware. If password is empty, authentication is disabled.
// The secret is derived from the password and used for JWT signing/verification.
// A rate limiter is set up: max 5 failed login attempts per 60-second window per IP.
func NewAuthMiddleware(password string) *AuthMiddleware {
	am := &AuthMiddleware{
		password: password,
		secret:   []byte(password),
	}
	am.limiter = newRateLimiter(5, 60*time.Second)
	return am
}

// SetPassword updates the password and JWT secret at runtime.
func (am *AuthMiddleware) SetPassword(password string) {
	am.password = password
	am.secret = []byte(password)
}

// SetSecret updates the JWT secret independently (used during initialization).
func (am *AuthMiddleware) SetSecret(secret []byte) {
	am.secret = secret
}

// CheckRateLimit checks if the given IP has exceeded the login rate limit.
// Returns true if the request should be rate-limited (429).
func (am *AuthMiddleware) CheckRateLimit(ip string) bool {
	if am.limiter == nil {
		return false
	}
	return am.limiter.record(ip)
}

// SignToken creates a JWT token for the given subject, valid for 5 minutes.
func (am *AuthMiddleware) SignToken(subject string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": subject,
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})
	return token.SignedString(am.secret)
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

// Authenticate checks all three auth methods: raw password, JWT header, JWT query param.
func (am *AuthMiddleware) Authenticate(r *http.Request) bool {
	// Collect all candidate tokens: Authorization header + query param
	var tokens []string

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		tokens = append(tokens, strings.TrimPrefix(auth, "Bearer "))
	}

	// SSE EventSource can't set headers, so support token query param
	queryToken := r.URL.Query().Get("token")
	if queryToken != "" {
		tokens = append(tokens, queryToken)
	}

	for _, token := range tokens {
		// Check 1: Raw password match (backwards compat for CLI)
		if token == am.password {
			return true
		}

		// Check 2: JWT validation
		if am.secret != nil && len(am.secret) > 0 {
			if parsed, err := jwt.Parse(token, func(*jwt.Token) (interface{}, error) {
				// Also accept the raw password as secret for backwards compat
				if am.password != "" {
					return []byte(am.password), nil
				}
				return am.secret, nil
			}, jwt.WithValidMethods([]string{"HS256"})); err == nil {
				if claims, ok := parsed.Claims.(jwt.MapClaims); ok && parsed.Valid {
					sub, _ := claims["sub"].(string)
					if sub == "admin" {
						return true
					}
				}
			}
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
