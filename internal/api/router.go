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
	cfg       *config.AppConfig
	authMW    *middleware.AuthMiddleware
}

// NewRouter creates an admin API router with all API endpoints.
func NewRouter(s *store.Store, eng *engine.Engine, cfg *config.AppConfig) *Router {
	return &Router{
		store:  s,
		engine: eng,
		cfg:    cfg,
		authMW: middleware.NewAuthMiddleware(cfg.AdminPassword),
	}
}

// Handler returns an http.Handler for the API routes only (under /api/).
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints — no auth required
	mux.HandleFunc("/api/auth/status", r.handleAuthStatus)
	mux.HandleFunc("/api/auth/login", r.handleAuthLogin)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

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
		case strings.HasPrefix(path, "aliases/"):
			r.handleAliasByID(w, req)
		case path == "plugins":
			r.handlePlugins(w, req)
		case path == "config":
			r.handleConfig(w, req)
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
	mux.Handle("/api/config", protected)

	return mux
}

// handleAuthStatus returns whether auth is enabled. Always accessible (no auth required).
func (r *Router) handleAuthStatus(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"auth_enabled": r.cfg.AdminPassword != "",
	})
}

// handleAuthLogin verifies the admin password. Always accessible (no auth required).
// Expects a JSON body: {"password": "..."}. Returns 200 on success, 401 on failure.
func (r *Router) handleAuthLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.cfg.AdminPassword == "" {
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

	if body.Password != r.cfg.AdminPassword {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
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
