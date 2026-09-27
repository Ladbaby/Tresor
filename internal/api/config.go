package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"tresor/internal/engine"
	"tresor/internal/proxy"
)

// ValidDefaultTabs lists the allowed tab IDs for DefaultTab.
var ValidDefaultTabs = []string{"dashboard", "downstreams", "aliases", "rules", "settings", "about"}

// RuntimeConfig exposes the mutable runtime settings via the admin API.
type RuntimeConfig struct {
	BindAddr        string   `json:"bind_addr"`
	ProxyMode       string   `json:"proxy_mode"`
	ProxyAPIKeys    []string `json:"proxy_api_keys"`
	AdminPassword   string   `json:"admin_password,omitempty"`
	DefaultTab      string   `json:"default_tab,omitempty"`
	LogLevel        string   `json:"log_level,omitempty"`
	CapturePayloads bool     `json:"capture_payloads,omitempty"`
	RetryOnEmpty    bool     `json:"retry_on_empty,omitempty"`
}

// RuntimeConfigResponse is what GET /api/config returns.
// The actual password is never sent back; we only indicate whether one is set.
type RuntimeConfigResponse struct {
	BindAddr         string   `json:"bind_addr"`
	ProxyMode        string   `json:"proxy_mode"`
	ProxyAPIKeys     []string `json:"proxy_api_keys"`
	AdminPasswordSet bool     `json:"admin_password_set"`
	DefaultTab       string   `json:"default_tab,omitempty"`
	LogLevel         string   `json:"log_level,omitempty"`
	CapturePayloads  bool     `json:"capture_payloads,omitempty"`
	RetryOnEmpty     bool     `json:"retry_on_empty,omitempty"`
}

var (
	runtimeCfg   = RuntimeConfig{ProxyMode: "auto"}
	runtimeCfgMu sync.RWMutex
)

// InitRuntimeConfig sets the initial runtime config from the YAML config so the
// API reflects what the engine was started with.
func InitRuntimeConfig(bindAddr string, mode string, proxyAPIKeys []string, adminPassword string, defaultTab string, logLevel string, capturePayloads bool, retryOnEmpty bool) {
	runtimeCfgMu.Lock()
	runtimeCfg.BindAddr = bindAddr
	runtimeCfg.ProxyMode = mode
	runtimeCfg.ProxyAPIKeys = proxyAPIKeys
	runtimeCfg.AdminPassword = adminPassword
	runtimeCfg.DefaultTab = defaultTab
	runtimeCfg.LogLevel = logLevel
	runtimeCfg.CapturePayloads = capturePayloads
	runtimeCfg.RetryOnEmpty = retryOnEmpty
	runtimeCfgMu.Unlock()
}

func (r *Router) handleConfig(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		runtimeCfgMu.RLock()
		cfg := runtimeCfg
		runtimeCfgMu.RUnlock()
		writeJSON(w, http.StatusOK, RuntimeConfigResponse{
			BindAddr:         cfg.BindAddr,
			ProxyMode:        cfg.ProxyMode,
			ProxyAPIKeys:     cfg.ProxyAPIKeys,
			AdminPasswordSet: cfg.AdminPassword != "",
			DefaultTab:       cfg.DefaultTab,
			LogLevel:         cfg.LogLevel,
			CapturePayloads:  cfg.CapturePayloads,
			RetryOnEmpty:     cfg.RetryOnEmpty,
		})

	case http.MethodPut:
		// Partial merge: only the fields present in the request body are
		// applied; everything else keeps its current value. This lets the
		// web UI auto-save a single field at a time (blur/change) without
		// disturbing the other settings.
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

		// Field presence rules (documented contract for PUT /api/config):
		//   - string fields: key present with a non-empty string → applied;
		//     an empty string means "unchanged"
		//   - admin_password: exception — a present string value (even "") is
		//     applied; "" clears the password
		//   - proxy_api_keys: array of strings (JSON null means unchanged)
		//   - bool fields: key present with a bool value → applied
		// A wrong type for any provided field is a 400.
		passwordProvided := false
		var newPassword string
		if v, ok := raw["admin_password"]; ok {
			s, isStr := v.(string)
			if !isStr {
				writeError(w, http.StatusBadRequest, "admin_password must be a string")
				return
			}
			passwordProvided = true
			newPassword = s
		}

		proxyKeysProvided := false
		var proxyKeys []string
		if v, ok := raw["proxy_api_keys"]; ok && v != nil {
			arr, isArr := v.([]interface{})
			if !isArr {
				writeError(w, http.StatusBadRequest, "proxy_api_keys must be an array of strings")
				return
			}
			proxyKeys = make([]string, 0, len(arr))
			for _, k := range arr {
				s, isStr := k.(string)
				if !isStr {
					writeError(w, http.StatusBadRequest, "proxy_api_keys elements must be strings")
					return
				}
				proxyKeys = append(proxyKeys, s)
			}
			proxyKeysProvided = true
		}

		proxyModeProvided := false
		var proxyModeVal string
		if v, ok := raw["proxy_mode"]; ok {
			s, isStr := v.(string)
			if !isStr {
				writeError(w, http.StatusBadRequest, "proxy_mode must be a string")
				return
			}
			if s != "" {
				proxyModeProvided = true
				proxyModeVal = s
			}
		}

		bindAddrProvided := false
		var bindAddrVal string
		if v, ok := raw["bind_addr"]; ok {
			s, isStr := v.(string)
			if !isStr {
				writeError(w, http.StatusBadRequest, "bind_addr must be a string")
				return
			}
			if s != "" {
				bindAddrProvided = true
				bindAddrVal = strings.TrimSpace(s)
			}
		}

		defaultTabProvided := false
		var defaultTabVal string
		if v, ok := raw["default_tab"]; ok {
			s, isStr := v.(string)
			if !isStr {
				writeError(w, http.StatusBadRequest, "default_tab must be a string")
				return
			}
			// "" is allowed here to reset to the default; validation below
			// accepts both known tabs and the empty string.
			defaultTabProvided = true
			defaultTabVal = s
		}

		logLevelProvided := false
		var logLevelVal string
		if v, ok := raw["log_level"]; ok {
			s, isStr := v.(string)
			if !isStr {
				writeError(w, http.StatusBadRequest, "log_level must be a string")
				return
			}
			if s != "" {
				logLevelProvided = true
				logLevelVal = s
			}
		}

		captureProvided := false
		var captureVal bool
		if v, ok := raw["capture_payloads"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				writeError(w, http.StatusBadRequest, "capture_payloads must be a boolean")
				return
			}
			captureProvided = true
			captureVal = b
		}

		retryProvided := false
		var retryVal bool
		if v, ok := raw["retry_on_empty"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				writeError(w, http.StatusBadRequest, "retry_on_empty must be a boolean")
				return
			}
			retryProvided = true
			retryVal = b
		}

		// Start from the current runtime config so omitted fields are
		// preserved verbatim.
		runtimeCfgMu.RLock()
		merged := runtimeCfg
		runtimeCfgMu.RUnlock()

		if bindAddrProvided {
			merged.BindAddr = bindAddrVal
		}
		if proxyModeProvided {
			merged.ProxyMode = proxyModeVal
		}
		if proxyKeysProvided {
			merged.ProxyAPIKeys = proxyKeys
		}
		if passwordProvided {
			merged.AdminPassword = newPassword
		}
		if defaultTabProvided {
			merged.DefaultTab = defaultTabVal
		}
		if logLevelProvided {
			merged.LogLevel = logLevelVal
		}
		if captureProvided {
			merged.CapturePayloads = captureVal
		}
		if retryProvided {
			merged.RetryOnEmpty = retryVal
		}

		// Validate bind_addr: must be a valid "host:port" pair.
		if bindAddrProvided {
			if _, _, err := net.SplitHostPort(merged.BindAddr); err != nil {
				writeError(w, http.StatusBadRequest, "invalid bind_addr; must be in the form \"host:port\" (e.g. \"127.0.0.1:11510\")")
				return
			}
		}

		// Validate proxy_mode value.
		mode := proxy.Mode(merged.ProxyMode)
		switch mode {
		case proxy.ModeAuto, proxy.ModeEnv, proxy.ModeWindows, proxy.ModeNone:
			// valid
		default:
			writeError(w, http.StatusBadRequest, "invalid proxy_mode; must be one of: auto, env, windows, none")
			return
		}

		// Validate default_tab value (empty string resets to default).
		if defaultTabProvided && merged.DefaultTab != "" {
			valid := false
			for _, tab := range ValidDefaultTabs {
				if merged.DefaultTab == tab {
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
		if logLevelProvided {
			logLevel, err := engine.ParseLogLevel(merged.LogLevel)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid log_level; must be one of: debug, info, warn, error")
				return
			}
			// Push the log level change to the logger live.
			r.logger.SetLevel(logLevel)
			r.logger.Debug("log level changed to %s", merged.LogLevel)
		}

		runtimeCfgMu.Lock()
		runtimeCfg = merged
		runtimeCfgMu.Unlock()

		// Push the change to the running engine live.
		r.engine.SetProxyMode(mode)
		if proxyKeysProvided {
			r.engine.SetProxyAuthKeys(merged.ProxyAPIKeys)
		}
		if r.oauthMgr != nil {
			r.oauthMgr.SetProxyMode(mode)
		}
		// Attach/detach the inspector's payload store to match the toggle.
		if captureProvided {
			if merged.CapturePayloads && r.payloadStore != nil {
				r.engine.SetPayloadStore(r.payloadStore)
			} else {
				r.engine.SetPayloadStore(nil)
			}
		}
		// Push retry-on-empty setting to the engine.
		if retryProvided {
			r.engine.SetRetryOnEmpty(merged.RetryOnEmpty)
		}
		if r.iconFetcher != nil {
			r.iconFetcher.SetProxyMode(mode)
		}

		// Update auth middleware password live (only when explicitly provided).
		if passwordProvided {
			r.authMW.SetPassword(merged.AdminPassword)
		}

		// Persist changed settings to YAML config (so they survive restart).
		if passwordProvided && r.cfg.AdminPassword != merged.AdminPassword {
			r.cfg.AdminPassword = merged.AdminPassword
			r.requestConfigWrite()
		}
		if proxyKeysProvided && !stringSlicesEqual(r.cfg.ProxyAPIKeys, merged.ProxyAPIKeys) {
			r.cfg.ProxyAPIKeys = merged.ProxyAPIKeys
			r.requestConfigWrite()
		}
		if proxyModeProvided && r.cfg.ProxyMode != merged.ProxyMode {
			r.cfg.ProxyMode = merged.ProxyMode
			r.requestConfigWrite()
		}
		if bindAddrProvided && r.cfg.BindAddr != merged.BindAddr {
			r.cfg.BindAddr = merged.BindAddr
			// bind_addr only takes effect on daemon restart, so flush the
			// YAML immediately (bypassing the debounce) — otherwise a user
			// who restarts right after saving would lose the change.
			r.writeConfigNow()
		}
		if defaultTabProvided && r.cfg.DefaultTab != merged.DefaultTab {
			r.cfg.DefaultTab = merged.DefaultTab
			r.requestConfigWrite()
		}
		if captureProvided && r.cfg.CapturePayloads != merged.CapturePayloads {
			r.cfg.CapturePayloads = merged.CapturePayloads
			r.requestConfigWrite()
		}
		if retryProvided && r.cfg.RetryOnEmpty != merged.RetryOnEmpty {
			r.cfg.RetryOnEmpty = merged.RetryOnEmpty
			r.requestConfigWrite()
		}
		if logLevelProvided && r.cfg.LogLevel != merged.LogLevel {
			r.cfg.LogLevel = merged.LogLevel
			r.requestConfigWrite()
		}

		writeJSON(w, http.StatusOK, RuntimeConfigResponse{
			BindAddr:         merged.BindAddr,
			ProxyMode:        merged.ProxyMode,
			ProxyAPIKeys:     merged.ProxyAPIKeys,
			AdminPasswordSet: merged.AdminPassword != "",
			DefaultTab:       merged.DefaultTab,
			LogLevel:         merged.LogLevel,
			CapturePayloads:  merged.CapturePayloads,
			RetryOnEmpty:     merged.RetryOnEmpty,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// stringSlicesEqual reports whether two string slices have identical elements
// in identical order (nil and empty slices are considered equal).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
