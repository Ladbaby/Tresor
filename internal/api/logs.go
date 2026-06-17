package api

import (
	"encoding/json"
	"net/http"

	"tresor/internal/engine"
)

// GetRecentLogs returns the most recent gateway request log entries.
// Auth is handled by the middleware (router.go), so no inline validation needed.
func GetRecentLogs(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries := logger.RecentEntries(100)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	}
}

// StreamLogs serves an SSE stream of gateway request logs.
// Auth is handled by the middleware (router.go) via auth cookie.
func StreamLogs(logger *engine.RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.StreamLogs(w, r)
	}
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

// SetLogLevel updates the log level for the request logger and syncs the global runtime config.
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

		// Sync the global runtime config (shared with handleConfig)
		runtimeCfgMu.Lock()
		runtimeCfg.LogLevel = req.Level
		runtimeCfgMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"level": logger.Level().String()})
	}
}
