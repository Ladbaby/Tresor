package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"tresor/internal/engine"
)

// GetRecentLogs returns the most recent gateway request log entries.
func GetRecentLogs(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Authenticate via token query param or Authorization header
		if err := validateSSEAuth(r); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		entries := logger.RecentEntries(100)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	}
}

// StreamLogs serves an SSE stream of gateway request logs.
func StreamLogs(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Authenticate via token query param (EventSource can't send headers)
		// or via Authorization header. Falls back to auth middleware config.
		if err := validateSSEAuth(r); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		logger.StreamLogs(w, r)
	}
}

// validateSSEAuth checks if the request has valid auth for SSE connections.
// Accepts token query param (for EventSource) or Authorization header.
func validateSSEAuth(r *http.Request) error {
	cfg := getRuntimeConfig()
	if cfg == nil || cfg.AdminPassword == "" {
		return nil // no auth required
	}

	// Check token query param (EventSource workaround)
	token := r.URL.Query().Get("token")
	if token != "" && token == cfg.AdminPassword {
		return nil
	}

	// Check Authorization header
	auth := r.Header.Get("Authorization")
	if auth != "" && len(auth) > 7 && auth[:7] == "Bearer " {
		if auth[7:] == cfg.AdminPassword {
			return nil
		}
	}

	return fmt.Errorf("unauthorized")
}

// GetLogLevel returns the current log level.
func GetLogLevel(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"level": logger.Level().String()})
	}
}

// SetLogLevel updates the log level for the request logger.
func SetLogLevel(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		level, err := engine.ParseLogLevel(req.Level)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		logger.SetLevel(level)

		// Also update the runtime config if available (for settings tab sync)
		if cfg := getRuntimeConfig(); cfg != nil {
			cfg.LogLevel = req.Level
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"level": logger.Level().String()})
	}
}

// Helper to access runtime config from log handlers for settings sync.
var runtimeConfigHolder *RuntimeConfig

// SetRuntimeConfig wires the runtime config pointer so log handlers can sync changes.
func SetRuntimeConfig(cfg *RuntimeConfig) {
	runtimeConfigHolder = cfg
}

func getRuntimeConfig() *RuntimeConfig {
	return runtimeConfigHolder
}
