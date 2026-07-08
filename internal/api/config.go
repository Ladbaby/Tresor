package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"tresor/internal/engine"
	"tresor/internal/proxy"
)

// ValidDefaultTabs lists the allowed tab IDs for DefaultTab.
var ValidDefaultTabs = []string{"downstreams", "aliases", "rules", "settings", "about"}

// RuntimeConfig exposes the mutable runtime settings via the admin API.
type RuntimeConfig struct {
	ProxyMode       string   `json:"proxy_mode"`
	ProxyAPIKeys    []string `json:"proxy_api_keys"`
	AdminPassword   string   `json:"admin_password,omitempty"`
	DefaultTab      string   `json:"default_tab,omitempty"`
	LogLevel        string   `json:"log_level,omitempty"`
	CapturePayloads bool     `json:"capture_payloads,omitempty"`
}

// RuntimeConfigResponse is what GET /api/config returns.
// The actual password is never sent back; we only indicate whether one is set.
type RuntimeConfigResponse struct {
	ProxyMode        string   `json:"proxy_mode"`
	ProxyAPIKeys     []string `json:"proxy_api_keys"`
	AdminPasswordSet bool     `json:"admin_password_set"`
	DefaultTab       string   `json:"default_tab,omitempty"`
	LogLevel         string   `json:"log_level,omitempty"`
	CapturePayloads  bool     `json:"capture_payloads,omitempty"`
}

var (
	runtimeCfg   = RuntimeConfig{ProxyMode: "auto"}
	runtimeCfgMu sync.RWMutex
)

// InitRuntimeConfig sets the initial runtime config from the YAML config so the
// API reflects what the engine was started with.
func InitRuntimeConfig(mode string, proxyAPIKeys []string, adminPassword string, defaultTab string, logLevel string, capturePayloads bool) {
	runtimeCfgMu.Lock()
	runtimeCfg.ProxyMode = mode
	runtimeCfg.ProxyAPIKeys = proxyAPIKeys
	runtimeCfg.AdminPassword = adminPassword
	runtimeCfg.DefaultTab = defaultTab
	runtimeCfg.LogLevel = logLevel
	runtimeCfg.CapturePayloads = capturePayloads
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
			CapturePayloads:  cfg.CapturePayloads,
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
			r.logger.Debug("log level changed to %s", incoming.LogLevel)
		}

		runtimeCfgMu.Lock()
		runtimeCfg.ProxyMode = incoming.ProxyMode
		runtimeCfg.ProxyAPIKeys = incoming.ProxyAPIKeys
		if passwordProvided {
			runtimeCfg.AdminPassword = incoming.AdminPassword
		}
		runtimeCfg.DefaultTab = incoming.DefaultTab
		runtimeCfg.LogLevel = incoming.LogLevel
		runtimeCfg.CapturePayloads = incoming.CapturePayloads
		runtimeCfgMu.Unlock()

		// Push the change to the running engine live.
		r.engine.SetProxyMode(mode)
		r.engine.SetProxyAuthKeys(incoming.ProxyAPIKeys)
		// Push the capture-payloads flag live; this controls whether the
		// engine snapshots raw request/response bodies for the inspector.
		r.engine.SetCapturePayloads(incoming.CapturePayloads)
		if r.iconFetcher != nil {
			r.iconFetcher.SetProxyMode(mode)
		}

		// Update auth middleware password live (only when explicitly provided).
		if passwordProvided {
			r.authMW.SetPassword(incoming.AdminPassword)
		}

		// Persist admin_password and capture_payloads to YAML config (so they
		// survive restart). capture_payloads is global system state, unlike
		// proxy_mode/proxy_api_keys/log_level/default_tab which are
		// environment-specific and not written back.
		if passwordProvided {
			r.cfg.AdminPassword = incoming.AdminPassword
			r.requestConfigWrite()
		}
		if r.cfg.CapturePayloads != incoming.CapturePayloads {
			r.cfg.CapturePayloads = incoming.CapturePayloads
			r.requestConfigWrite()
		}

		writeJSON(w, http.StatusOK, RuntimeConfigResponse{
			ProxyMode:        incoming.ProxyMode,
			ProxyAPIKeys:     incoming.ProxyAPIKeys,
			AdminPasswordSet: passwordProvided && incoming.AdminPassword != "",
			DefaultTab:       incoming.DefaultTab,
			LogLevel:         incoming.LogLevel,
			CapturePayloads:  incoming.CapturePayloads,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
