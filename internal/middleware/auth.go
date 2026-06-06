package middleware

import (
	"net/http"
)

// AuthMiddleware provides optional bearer-token authentication for admin API endpoints.
type AuthMiddleware struct {
	password string
}

// NewAuthMiddleware creates an auth middleware. If password is empty, authentication is disabled.
func NewAuthMiddleware(password string) *AuthMiddleware {
	return &AuthMiddleware{password: password}
}

// Protect wraps an http.Handler with password-based bearer token authentication.
// If no password is configured, it passes through all requests.
// SetPassword updates the password at runtime.
func (am *AuthMiddleware) SetPassword(password string) {
	am.password = password
}

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
		token := r.Header.Get("Authorization")
		expected := "Bearer " + am.password
		if token != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
