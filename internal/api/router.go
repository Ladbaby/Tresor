package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"tresor/internal/config"
	"tresor/internal/engine"
	"tresor/internal/middleware"
	"tresor/internal/store"
)

// Router holds dependencies for the admin API handlers.
type Router struct {
	store     *store.Store
	engine    *engine.Engine
	logger    *engine.RequestLogger
	cfg       *config.AppConfig
	authMW    *middleware.AuthMiddleware
	version   string
	buildTime string
}

// NewRouter creates an admin API router with all API endpoints.
func NewRouter(s *store.Store, eng *engine.Engine, logger *engine.RequestLogger, cfg *config.AppConfig, version, buildTime string) *Router {
	am := middleware.NewAuthMiddleware(cfg.AdminPassword)
	if cfg.JWTSecret != nil {
		am.SetSecret(cfg.JWTSecret)
	}
	return &Router{
		store:     s,
		engine:    eng,
		logger:    logger,
		cfg:       cfg,
		authMW:    am,
		version:   version,
		buildTime: buildTime,
	}
}

// Handler returns an http.Handler for the API routes only (under /api/).
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints — no auth required
	mux.HandleFunc("/api/auth/status", r.handleAuthStatus)
	mux.HandleFunc("/api/auth/login", r.handleAuthLogin)
	mux.HandleFunc("/api/auth/refresh", r.handleAuthRefresh)
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
		case path == "log_level":
			SetLogLevel(r.logger)(w, req)
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

// handleAuthLogin verifies the admin password and issues a JWT token.
// Expects a JSON body: {"password": "..."}. Returns {"ok": true, "token": "<jwt>"} on success, 401 on failure.
func (r *Router) handleAuthLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.cfg.AdminPassword == "" {
		token, _ := r.authMW.SignToken("admin")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    true,
			"token": token,
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

	if body.Password != r.cfg.AdminPassword {
		// Record failed attempt; check if rate limited
		if r.authMW.CheckRateLimit(middleware.ExtractClientIP(req)) {
			writeError(w, http.StatusTooManyRequests, "too many login attempts, please wait")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	token, err := r.authMW.SignToken("admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"token": token,
	})
}

// handleAuthRefresh issues a new JWT token if the current token is valid.
// Only useful for SSE EventSource connections that can't set Authorization headers.
func (r *Router) handleAuthRefresh(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.cfg.AdminPassword == "" {
		token, _ := r.authMW.SignToken("admin")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    true,
			"token": token,
		})
		return
	}

	// Require a valid token in the Authorization header to refresh
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "missing Bearer token")
		return
	}
	if !r.authMW.Authenticate(req) {
		writeError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}

	token, err := r.authMW.SignToken("admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"token": token,
	})
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