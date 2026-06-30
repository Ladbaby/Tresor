package api

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"tresor/internal/config"
	"tresor/internal/engine"
	"tresor/internal/icons"
	"tresor/internal/middleware"
	"tresor/internal/store"
)

// setAuthCookie is a convenience wrapper for the middleware cookie function.
func setAuthCookie(w http.ResponseWriter, token string) {
	middleware.SetAuthCookie(w, token)
}

// clearAuthCookie is a convenience wrapper for the middleware cookie function.
func clearAuthCookie(w http.ResponseWriter) {
	middleware.ClearAuthCookie(w)
}

// Router holds dependencies for the admin API handlers.
type Router struct {
	store     *store.Store
	engine    *engine.Engine
	logger    *engine.RequestLogger
	cfg       *config.AppConfig
	authMW    *middleware.AuthMiddleware
	iconFetcher *icons.Fetcher
	version   string
	buildTime string

	// Config write debounce: delays YAML write-back to avoid excessive disk I/O
	// when multiple mutations occur in rapid succession.
	configDebounceTimer *time.Timer
	configDebounceMu    sync.Mutex
}

// NewRouter creates an admin API router with all API endpoints.
// It wires up session token persistence so all auth cookies survive daemon restarts.
// iconFetcher may be nil in tests; when nil, /api/icons/ responds with 404.
func NewRouter(s *store.Store, eng *engine.Engine, logger *engine.RequestLogger, iconFetcher *icons.Fetcher, cfg *config.AppConfig, version, buildTime string) *Router {
	authMW := middleware.NewAuthMiddleware(cfg.AdminPassword)

	// Wire persistence hooks so every session token is saved to SQLite,
	// and individual tokens can be removed without disturbing the others.
	authMW.OnTokenGenerated = func(token string) error {
		return s.SaveSessionToken(token)
	}
	authMW.OnTokenCleared = func(token string) error {
		return s.DeleteSessionToken(token)
	}

	// Restore every existing session token from the database so browsers that
	// were logged in before the daemon restarted stay logged in.
	if tokens, err := s.LoadAllSessionTokens(); err == nil {
		authMW.SetTokens(tokens)
	}

	return &Router{
		store:       s,
		engine:      eng,
		logger:      logger,
		cfg:         cfg,
		authMW:      authMW,
		iconFetcher: iconFetcher,
		version:     version,
		buildTime:   buildTime,
	}
}

// Stop cleans up background goroutines (rate limiter, debounce timer).
func (r *Router) Stop() {
	if r.authMW != nil {
		r.authMW.Stop()
	}
	r.configDebounceMu.Lock()
	if r.configDebounceTimer != nil {
		r.configDebounceTimer.Stop()
	}
	r.configDebounceMu.Unlock()
}

// requestConfigWrite schedules a debounced YAML write-back.
// Subsequent calls within the debounce window reset the timer.
func (r *Router) requestConfigWrite() {
	r.configDebounceMu.Lock()
	if r.configDebounceTimer != nil {
		r.configDebounceTimer.Stop()
	}
	r.configDebounceTimer = time.AfterFunc(2*time.Second, func() {
		if err := r.store.WriteConfig(r.cfg); err != nil {
			log.Printf("warning: failed to write config YAML: %v", err)
		}
		r.configDebounceMu.Lock()
		r.configDebounceTimer = nil
		r.configDebounceMu.Unlock()
	})
	r.configDebounceMu.Unlock()
}

// Handler returns an http.Handler for the API routes only (under /api/).
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints — no auth required
	mux.HandleFunc("/api/auth/status", r.handleAuthStatus)
	mux.HandleFunc("/api/auth/login", r.handleAuthLogin)
	mux.HandleFunc("/api/auth/logout", r.handleAuthLogout)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":   r.version,
			"build_time": r.buildTime,
		})
	})

	// Public icon endpoint — no auth required, hits in <img> tags. Trailing
	// slash is required so ServeMux dispatches /api/icons/{modelID} here.
	mux.HandleFunc("/api/icons/", r.handleIcon)

	mux.Handle("/api/log_level", r.authMW.Protect(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			GetLogLevel(r.logger)(w, req)
		} else {
			SetLogLevel(r.logger)(w, req)
		}
	})))

	// Protected admin routes
	protected := r.authMW.Protect(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Strip /api prefix and dispatch to protected handlers
		path := strings.TrimPrefix(req.URL.Path, "/api/")

		switch {
		case path == "rules":
			r.handleRules(w, req)
		case strings.HasPrefix(path, "rules/"):
			r.handleRuleByID(w, req)
		case path == "downstreams":
			r.handleDownstreams(w, req)
		case strings.HasPrefix(path, "downstreams/"):
			r.handleDownstreamByID(w, req)
		case strings.HasPrefix(path, "aliases/group/"):
			r.handleAliasGroup(w, req)
		case path == "aliases":
			r.handleAliases(w, req)
		case path == "aliases/reorder":
			r.handleReorderGroups(w, req)
		case strings.HasPrefix(path, "aliases/"):
			r.handleAliasByID(w, req)
		case path == "plugins":
			r.handlePlugins(w, req)
		case path == "fetch-models":
			r.handleFetchModels(w, req)
		case path == "config":
			r.handleConfig(w, req)
		case path == "icons/refresh":
			r.handleIconRefresh(w, req)
		case path == "logs/stream":
			StreamLogs(r.logger)(w, req)
		case path == "logs":
			GetRecentLogs(r.logger)(w, req)
		default:
			http.NotFound(w, req)
		}
	}))

	mux.Handle("/api/rules", protected)
	mux.Handle("/api/rules/", protected)
	mux.Handle("/api/downstreams", protected)
	mux.Handle("/api/downstreams/", protected)
	mux.Handle("/api/aliases/group/", protected)
	mux.Handle("/api/aliases", protected)
	mux.Handle("/api/aliases/", protected)
	mux.Handle("/api/plugins", protected)
	mux.Handle("/api/fetch-models", protected)
	mux.Handle("/api/config", protected)
	mux.Handle("/api/icons/refresh", protected)
	mux.Handle("/api/logs/stream", protected)
	mux.Handle("/api/logs", protected)

	return mux
}

// handleAuthStatus returns whether auth is enabled. Always accessible (no auth required).
func (r *Router) handleAuthStatus(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"auth_enabled": r.cfg.AdminPassword != "",
	})
}

// handleAuthLogin verifies the admin password and issues a session token.
// Expects a JSON body: {"password": "..."}. Returns {"ok": true} on success, 401 on failure.
// Sets the auth cookie (persistent, 365-day expiry) for all subsequent requests.
func (r *Router) handleAuthLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.cfg.AdminPassword == "" {
		token := r.authMW.GenerateToken()
		setAuthCookie(w, token)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true,
		})
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(r.cfg.AdminPassword)) != 1 {
		// Record failed attempt; check if rate limited
		if r.authMW.CheckRateLimit(middleware.ExtractClientIP(req)) {
			writeError(w, http.StatusTooManyRequests, "too many login attempts, please wait")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	token := r.authMW.GenerateToken()
	setAuthCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
	})
}

// handleAuthLogout clears the caller's session cookie and revokes only that
// session. Other browsers/devices remain logged in. If no session token is
// attached (no cookie, no Bearer), we just expire the cookie and return OK —
// we don't know which session to revoke, and we must not nuke everyone else's.
func (r *Router) handleAuthLogout(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if token := callerSessionToken(req); token != "" {
		r.authMW.ClearToken(token)
	}
	clearAuthCookie(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// callerSessionToken returns the session token attached to the request, if any.
// Cookie is checked first (the normal web UI path); Bearer header is the
// fallback (matches what Authenticate accepts). The Bearer branch returns
// the bearer value even if it happens to equal the admin password — passing
// it to ClearToken is a harmless no-op since the password is never inserted
// into the token set.
func callerSessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(middleware.AuthCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// UDSHandler returns an http.Handler that serves both the API and Web UI (for Unix sockets).
func (r *Router) UDSHandler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	apiHandler := r.Handler()
	mux.Handle("/api/", apiHandler)

	// Web UI
	mux.Handle("/", WebHandler())

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeJSONWithWarning writes a downstream as JSON, appending a bare-IP warning
// when isBareIP is true. Downstream fields are at the top level so existing
// consumers (and tests) can decode directly into store.Downstream; the extra
// "warning" key is simply ignored by the decoder.
func writeJSONWithWarning(w http.ResponseWriter, status int, ds store.Downstream, isBareIP bool) {
	type downstreamWithWarning struct {
		store.Downstream
		Warning string `json:"warning,omitempty"`
	}
	dw := downstreamWithWarning{Downstream: ds}
	if isBareIP {
		dw.Warning = "base_url uses a bare IP address: traffic is sent unencrypted and may be interceptible"
	}
	writeJSON(w, status, dw)
}