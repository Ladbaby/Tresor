package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"tresor/internal/engine"
	"tresor/internal/proxy"
)

// ValidDefaultTabs lists the allowed tab IDs for DefaultTab.
var ValidDefaultTabs = []string{"downstreams", "aliases", "rules", "plugins", "settings", "about"}

// RuntimeConfig exposes the mutable runtime settings via the admin API.
type RuntimeConfig struct {
	ProxyMode     string   `json:"proxy_mode"`
	ProxyAPIKeys  []string `json:"proxy_api_keys"`
	AdminPassword string   `json:"admin_password,omitempty"`
	DefaultTab    string   `json:"default_tab,omitempty"`
	LogLevel      string   `json:"log_level,omitempty"`
}

// RuntimeConfigResponse is what GET /api/config returns.
// The actual password is never sent back; we only indicate whether one is set.
type RuntimeConfigResponse struct {
	ProxyMode        string   `json:"proxy_mode"`
	ProxyAPIKeys     []string `json:"proxy_api_keys"`
	AdminPasswordSet bool     `json:"admin_password_set"`
	DefaultTab       string   `json:"default_tab,omitempty"`
	LogLevel         string   `json:"log_level,omitempty"`
}

var (
	runtimeCfg   = RuntimeConfig{ProxyMode: "auto"}
	runtimeCfgMu sync.RWMutex
)

// InitRuntimeConfig sets the initial runtime config from the YAML config so the
// API reflects what the engine was started with.
func InitRuntimeConfig(mode string, proxyAPIKeys []string, adminPassword string, defaultTab string, logLevel string) {
	runtimeCfgMu.Lock()
	runtimeCfg.ProxyMode = mode
	runtimeCfg.ProxyAPIKeys = proxyAPIKeys
	runtimeCfg.AdminPassword = adminPassword
	runtimeCfg.DefaultTab = defaultTab
	runtimeCfg.LogLevel = logLevel
	runtimeCfgMu.Unlock()
}

func (r *Router) handleConfig(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		runtimeCfgMu.RLock()
		cfg := runtimeCfg
		runtimeCfgMu.RUnlock()
		writeJSON(w, http.StatusOK, RuntimeConfigResponse{
			ProxyMode:        cfg.ProxyMode,
			ProxyAPIKeys:     cfg.ProxyAPIKeys,
			AdminPasswordSet: cfg.AdminPassword != "",
			DefaultTab:       cfg.DefaultTab,
			LogLevel:         cfg.LogLevel,
		})

	case http.MethodPut:
		// Parse raw JSON first to determine which fields were provided.
		var raw map[string]interface{}
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := json.Unmarshal(bodyBytes, &raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Check if admin_password was explicitly provided in the request.
		passwordProvided := raw["admin_password"] != nil

		var incoming RuntimeConfig
		if err := json.Unmarshal(bodyBytes, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Validate proxy_mode value.
		mode := proxy.Mode(incoming.ProxyMode)
		switch mode {
		case proxy.ModeAuto, proxy.ModeEnv, proxy.ModeWindows, proxy.ModeNone:
			// valid
		default:
			writeError(w, http.StatusBadRequest, "invalid proxy_mode; must be one of: auto, env, windows, none")
			return
		}

		// Validate default_tab value.
		if incoming.DefaultTab != "" {
			valid := false
			for _, tab := range ValidDefaultTabs {
				if incoming.DefaultTab == tab {
					valid = true
					break
				}
			}
			if !valid {
				writeError(w, http.StatusBadRequest, "invalid default_tab; must be one of: "+strings.Join(ValidDefaultTabs, ", "))
				return
			}
		}

		// Validate log_level value.
		if incoming.LogLevel != "" {
			logLevel, err := engine.ParseLogLevel(incoming.LogLevel)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid log_level; must be one of: debug, info, warn, error")
				return
			}
			// Push the log level change to the logger live.
			r.logger.SetLevel(logLevel)
		}

		runtimeCfgMu.Lock()
		runtimeCfg.ProxyMode = incoming.ProxyMode
		runtimeCfg.ProxyAPIKeys = incoming.ProxyAPIKeys
		if passwordProvided {
			runtimeCfg.AdminPassword = incoming.AdminPassword
		}
		runtimeCfg.DefaultTab = incoming.DefaultTab
		runtimeCfg.LogLevel = incoming.LogLevel
		runtimeCfgMu.Unlock()

		// Push the change to the running engine live.
		r.engine.SetProxyMode(mode)
		r.engine.SetProxyAuthKeys(incoming.ProxyAPIKeys)

		// Update auth middleware password live (only when explicitly provided).
		if passwordProvided {
			r.authMW.SetPassword(incoming.AdminPassword)
		}

		// Persist server settings back to the YAML config file so they
		// survive a daemon restart. Update the shared AppConfig pointer and
		// trigger an async write-back (best-effort, same pattern as aliases).
		r.cfg.ProxyMode = incoming.ProxyMode
		r.cfg.ProxyAPIKeys = incoming.ProxyAPIKeys
		if passwordProvided {
			r.cfg.AdminPassword = incoming.AdminPassword
		}
		r.cfg.DefaultTab = incoming.DefaultTab
		r.cfg.LogLevel = incoming.LogLevel
		go func() {
			if err := r.store.WriteConfig(r.cfg); err != nil {
				log.Printf("warning: failed to write config YAML: %v", err)
			}
		}()

		writeJSON(w, http.StatusOK, RuntimeConfigResponse{
			ProxyMode:        incoming.ProxyMode,
			ProxyAPIKeys:     incoming.ProxyAPIKeys,
			AdminPasswordSet: passwordProvided && incoming.AdminPassword != "",
			DefaultTab:       incoming.DefaultTab,
			LogLevel:         incoming.LogLevel,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
