package engine

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"tresor/internal/middleware"
)

// proxyAuth holds the set of allowed API keys for incoming proxy requests.
type proxyAuth struct {
	enabled bool
	keys    map[string]struct{}
}

// validateProxyAuth checks the proxy API key sent by the client, supporting
// Authorization: Bearer <key>, x-api-key: <key>, and x-goog-api-key: <key> headers
// (the latter two are used by Anthropic- and Gemini-format clients respectively),
// and the ?key=<key> query parameter on Gemini paths (/v1beta/*).
// If auth is enabled and the key is invalid, it writes a 401 response and returns false.
// On success (or when auth is disabled), it returns true and strips the auth header
// from the request so the downstream's own API key can be injected cleanly.
func (e *Engine) validateProxyAuth(r *http.Request, w http.ResponseWriter) bool {
	if e.proxyAuth == nil || !e.proxyAuth.enabled {
		return true
	}

	// Try Authorization: Bearer <key>, then x-api-key: <key>, then x-goog-api-key: <key>
	token := ""
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimPrefix(authHeader, "Bearer ")
	}
	if token == "" {
		if xak := r.Header.Get("x-api-key"); xak != "" {
			token = xak
		}
	}
	if token == "" {
		if xgak := r.Header.Get("x-goog-api-key"); xgak != "" {
			token = xgak
		}
	}
	// Gemini paths also accept the key via the ?key=<token> query parameter.
	// Cherry Studio (and Google's own SDKs) use this form. Only honor it on
	// Gemini paths so other formats can't smuggle credentials through the URL.
	queryHadKey := false
	if token == "" && strings.HasPrefix(r.URL.Path, "/v1beta/") {
		if qk := r.URL.Query().Get("key"); qk != "" {
			token = qk
			queryHadKey = true
		}
	}

	if _, ok := e.proxyAuth.keys[token]; !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "unauthorized",
			"message": "invalid or missing proxy API key",
		})
		return false
	}

	// Strip the client's auth header so it doesn't leak to downstream.
	// The downstream will receive its own API key from config.
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	r.Header.Del("x-goog-api-key")
	// Strip ?key= from the URL so the proxy key isn't forwarded to the downstream.
	if queryHadKey {
		q := r.URL.Query()
		q.Del("key")
		r.URL.RawQuery = q.Encode()
	}

	return true
}

// ServeProxy starts an HTTP server that serves both the admin API and the LLM gateway.
// It uses a mux that routes /api/* to the admin handler and everything else to the proxy.
// webHandler serves the embedded web UI at the root.
// isWebPath is a function that checks if a path belongs to the web UI.
func ServeProxy(eng *Engine, adminHandler http.Handler, webHandler http.Handler, isWebPath func(string) bool, listener net.Listener) error {
	mux := http.NewServeMux()

	// Admin API routes (wrapped with security headers)
	mux.Handle("/api/", middleware.SecurityHeaders(adminHandler))

	// Integration control and binding resolution are gateway endpoints, outside admin auth.
	mux.Handle("/api/integrations/bindings/resolve", http.HandlerFunc(eng.HandleIntegrationBindingResolve))
	mux.Handle("/api/integrations/bindings/resolve/", http.HandlerFunc(eng.HandleIntegrationBindingResolve))

	// Everything else: web UI for known paths, proxy for API-like paths
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			webHandler.ServeHTTP(w, r)
			return
		}
		// Check if it's a web UI asset
		if isWebPath != nil && isWebPath(r.URL.Path) {
			webHandler.ServeHTTP(w, r)
			return
		}
		// Otherwise proxy the request
		eng.HandleProxy(w, r)
	}))

	return http.Serve(listener, middleware.SecurityHeaders(mux))
}

// ServeAdminOnly starts an HTTP server that serves both the admin API and web UI (for UDS).
func ServeAdminOnly(adminHandler http.Handler, listener net.Listener) error {
	return http.Serve(listener, middleware.SecurityHeaders(adminHandler))
}

// corsHeaders writes CORS headers into the response. These are needed so that
// browser-based LLM clients (e.g. Claude plugin webviews) can make cross-origin
// requests to the gateway. We list all headers used by the Anthropic SDK/Stainless
// library so CORS preflights don't reject the actual request.
func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-dangerous-direct-browser-access, x-stainless-arch, x-stainless-helper-method, x-stainless-lang, x-stainless-os, x-stainless-package-version, x-stainless-retry-count, x-stainless-runtime, x-stainless-runtime-version, x-stainless-timeout")
}
