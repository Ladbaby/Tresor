package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// AppConfig is the complete YAML configuration for Tresor.
// It covers server settings and all routing data (downstreams, rules, aliases).
type AppConfig struct {
	// Server settings
	BindAddr      string `yaml:"bind_addr"`
	SocketPath    string `yaml:"socket_path,omitempty"`
	DBPath        string `yaml:"db_path"`
	AdminPassword string `yaml:"admin_password,omitempty"`

	// ProxyMode controls how outbound requests to downstreams are proxied.
	// Values: "auto" (default, Windows registry > env vars), "env" (env vars only),
	// "windows" (Windows registry + env fallback), "none" (direct connection).
	ProxyMode string `yaml:"proxy_mode,omitempty"`

	// ProxyAPIKeys is a list of allowed API keys for incoming proxy requests.
	// When set, clients must send "Authorization: Bearer <key>" to use the proxy.
	// Empty or omitted = no authentication required (backwards compatible).
	ProxyAPIKeys []string `yaml:"proxy_api_keys,omitempty"`

	// DefaultTab is the dashboard tab shown on UI load ("downstreams", "aliases",
	// "rules", "plugins", or "settings"). Empty string = "downstreams" (default).
	DefaultTab string `yaml:"default_tab,omitempty"`

	// LogLevel controls request logging verbosity. Values: debug, info, warn, error.
	// Entries below the selected level are filtered out. Default: info.
	LogLevel string `yaml:"log_level,omitempty"`

	// CapturePayloads enables capture of the raw incoming request body and the
	// raw downstream response body for the most recent N requests (default 100)
	// in memory. Captured bytes are exposed via GET /api/logs/{id}/inspect.
	// This adds a small per-request memory cost (~tens of KB per direction for
	// typical LLM payloads) and is therefore disabled by default.
	CapturePayloads bool `yaml:"capture_payloads,omitempty"`

	// IconCacheDir is the directory where downloaded model icon SVGs are
	// stored. If empty, defaults to <db_dir>/tresor-icons/ (the directory
	// holding the SQLite DB). Tilde (~) is expanded to the user's home dir.
	IconCacheDir string `yaml:"icon_cache_dir,omitempty"`

	// RetryOnEmpty enables automatic retry when a downstream returns an empty
	// response (no content generated). When enabled, the gateway will re-send
	// the request with exponential backoff (up to 3 retries) if the downstream
	// LLM produces no content (e.g., empty choices, no text blocks).
	RetryOnEmpty bool `yaml:"retry_on_empty,omitempty"`

	// legacyOAuthProviders parses the old top-level oauth_providers section for
	// one-shot back-compat migration. It is private so it is never written
	// back to YAML; at startup its recipes are merged into the matching
	// downstreams' auth blocks and then dropped.
	legacyOAuthProviders []OAuthProviderCfg `yaml:"oauth_providers,omitempty"`

	// Routing data (loaded into SQLite at startup via upsert)
	Downstreams []DownstreamCfg `yaml:"downstreams"`
	Rules       []RuleCfg       `yaml:"rules"`
	Aliases     []AliasGroupCfg `yaml:"aliases"`

	// ConfigPath is the resolved file path of the YAML config.
	// Not serialized to YAML; used for write-back on mutations.
	ConfigPath string `yaml:"-"`
}

// DownstreamCfg defines a downstream LLM provider endpoint.
type DownstreamCfg struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	BaseURL        string   `yaml:"base_url"`
	ApiFormats     []string `yaml:"api_formats,omitempty"`
	OutputModelIDs []string `yaml:"output_model_ids,omitempty"`

	// IsEnabled controls whether this downstream is available. When false the
	// downstream and its models behave "as if deleted". A pointer so that an
	// absent field in existing YAML loads as nil (= enabled) rather than
	// silently disabling every provider; only an explicit false disables.
	IsEnabled *bool `yaml:"is_enabled,omitempty"`

	// Auth holds the per-downstream authentication configuration (API key or
	// OAuth). See DownstreamAuthCfg. When omitted in YAML, Load synthesizes
	// an api_key auth from the legacy top-level api_key field.
	Auth *DownstreamAuthCfg `yaml:"auth,omitempty"`

	// legacyAPIKey reads the old top-level api_key field for one-shot back-
	// compat. Private so it is never written back; folded into Auth in Load.
	legacyAPIKey string `yaml:"api_key,omitempty"`

	// FormatURLs maps API format names (openai, openai_responses, anthropic, gemini)
	// to per-format base URLs. When set, the engine uses the format-specific URL
	// for requests in that format, falling back to BaseURL for formats without
	// an override. Useful for providers that serve different API formats at
	// different endpoints (e.g., DeepSeek serves Anthropic at /anthropic but
	// OpenAI at the root URL).
	FormatURLs map[string]string `yaml:"format_urls,omitempty"`

	// FormatPaths maps API format names to the request path used when
	// forwarding in that format (e.g. openai_responses -> "/responses"). When
	// set for a format the outbound path is base_url + format_path (the
	// client's own path is ignored); when unset the engine appends the
	// client's path. Useful for providers whose endpoint lives at a
	// non-standard path (e.g. ChatGPT Codex serves Responses at /responses,
	// not /v1/responses).
	FormatPaths map[string]string `yaml:"format_paths,omitempty"`

	// ApiFormat is a legacy field accepted for backward-compatible YAML loading.
	// It gets converted to ApiFormats during the sanitize step.
	ApiFormat string `yaml:"api_format,omitempty"`
}

// DownstreamAuthCfg is the per-downstream authentication configuration. It
// unifies the old separate api_key field and the old top-level oauth_providers
// recipe into one nested object so every auth field is editable in the web UI
// and round-trips through the YAML config.
//
// Type selects the auth method:
//   - "api_key" (default): requests are authenticated with APIKey.
//   - "oauth": requests are authenticated with a managed OAuth token, obtained
//     and refreshed per the flow below. Tokens live in SQLite, never YAML.
//
// Flow (only for oauth) selects the OAuth flow:
//   - "auth_code": Authorization Code + PKCE (RFC 7636) with a loopback
//     redirect. Requires AuthorizationURL (or DiscoveryURL) and TokenURL
//     (or DiscoveryURL).
//   - "device": RFC 8628 Device Code flow. Requires DeviceAuthURL and
//     DeviceTokenURL.
type DownstreamAuthCfg struct {
	Type string `yaml:"type" json:"type"` // "api_key" (default) | "oauth"

	// api_key method
	APIKey string `yaml:"api_key,omitempty" json:"api_key,omitempty"`

	// oauth — flow
	Flow string `yaml:"flow,omitempty" json:"flow,omitempty"` // "auth_code" | "device"

	// oauth — auth_code flow
	AuthorizationURL string `yaml:"authorization_url,omitempty" json:"authorization_url,omitempty"` // required unless DiscoveryURL set
	TokenURL         string `yaml:"token_url,omitempty" json:"token_url,omitempty"`                 // required unless DiscoveryURL set
	DiscoveryURL     string `yaml:"discovery_url,omitempty" json:"discovery_url,omitempty"`         // OIDC .well-known; fills AuthorizationURL + TokenURL
	RedirectPort     int    `yaml:"redirect_port,omitempty" json:"redirect_port,omitempty"`         // auth_code: default 56120
	RedirectPath     string `yaml:"redirect_path,omitempty" json:"redirect_path,omitempty"`         // auth_code: default "/callback"

	// oauth — device flow
	DeviceAuthURL          string `yaml:"device_auth_url,omitempty" json:"device_auth_url,omitempty"`         // request device code
	DeviceTokenURL         string `yaml:"device_token_url,omitempty" json:"device_token_url,omitempty"`       // poll for the authorization grant
	DeviceVerifyURL        string `yaml:"device_verify_url,omitempty" json:"device_verify_url,omitempty"`     // the URL the user visits to enter the code
	DeviceExchangeRedirect string `yaml:"device_redirect_uri,omitempty" json:"device_redirect_uri,omitempty"` // optional redirect_uri sent when exchanging the device authorization code

	// oauth — common
	ClientID        string            `yaml:"client_id,omitempty" json:"client_id,omitempty"`                  // optional for public clients
	ClientSecret    string            `yaml:"client_secret,omitempty" json:"client_secret,omitempty"`          // required for confidential clients (sent as Basic auth on token requests)
	Scopes          string            `yaml:"scopes,omitempty" json:"scopes,omitempty"`                        // space-separated, optional
	ExtraAuthParams map[string]string `yaml:"extra_authorize_params,omitempty" json:"extra_authorize_params,omitempty"` // extra query params appended to the authorize URL
	RefreshSkewSecs int               `yaml:"refresh_skew_secs,omitempty" json:"refresh_skew_secs,omitempty"`   // refresh ahead of expiry, default 120
	ExtraHeaders    map[string]string `yaml:"extra_headers,omitempty" json:"extra_headers,omitempty"`           // static extra headers on LLM requests

	// OAuthProvider is a transient field used only during startup migration of
	// old configs: it carries the name of the recipe in the old top-level
	// oauth_providers list that this downstream referenced. It is never
	// serialized to YAML and is cleared after migration.
	OAuthProvider string `yaml:"-" json:"oauth_provider,omitempty"`
}

// LegacyOAuthProviders returns the old top-level oauth_providers entries
// parsed from the YAML (empty when absent). Used only for the one-shot
// startup migration that folds recipes into per-downstream auth blocks.
func (c *AppConfig) LegacyOAuthProviders() []OAuthProviderCfg {
	return c.legacyOAuthProviders
}

// SetLegacyOAuthProviders replaces the parsed legacy oauth_providers list.
// Used at the end of the one-shot migration to clear the list so it is not
// written back to the YAML.
func (c *AppConfig) SetLegacyOAuthProviders(entries []OAuthProviderCfg) {
	c.legacyOAuthProviders = entries
}

// legacyOAuthProviderCfg is the shape of the old top-level oauth_providers
// entries, retained only so existing configs can be migrated. It is
// deliberately separate from DownstreamAuthCfg (which has no Name) so old
// YAML keeps parsing while the new per-downstream model takes over.
type OAuthProviderCfg struct {
	Name string `yaml:"name"`
	Flow string `yaml:"flow"`

	// auth_code flow
	AuthorizationURL string `yaml:"authorization_url,omitempty"`
	TokenURL         string `yaml:"token_url,omitempty"`
	DiscoveryURL     string `yaml:"discovery_url,omitempty"`

	// device flow
	DeviceAuthURL          string `yaml:"device_auth_url,omitempty"`
	DeviceTokenURL         string `yaml:"device_token_url,omitempty"`
	DeviceVerifyURL        string `yaml:"device_verify_url,omitempty"`
	DeviceExchangeRedirect string `yaml:"device_redirect_uri,omitempty"`

	// Common
	ClientID        string            `yaml:"client_id,omitempty"`
	Scopes          string            `yaml:"scopes,omitempty"`
	RedirectPort    int               `yaml:"redirect_port,omitempty"`
	RedirectPath    string            `yaml:"redirect_path,omitempty"`
	RefreshSkewSecs int               `yaml:"refresh_skew_secs,omitempty"`
	ExtraHeaders    map[string]string `yaml:"extra_headers,omitempty"`
}

// ToAuthCfg converts a legacy top-level OAuth provider recipe into a per-
// downstream DownstreamAuthCfg (type=oauth). The Name is dropped — the
// downstream it is folded into carries its own identity.
func (p OAuthProviderCfg) ToAuthCfg() *DownstreamAuthCfg {
	return &DownstreamAuthCfg{
		Type:                   "oauth",
		Flow:                   p.Flow,
		AuthorizationURL:       p.AuthorizationURL,
		TokenURL:               p.TokenURL,
		DiscoveryURL:           p.DiscoveryURL,
		RedirectPort:           p.RedirectPort,
		RedirectPath:           p.RedirectPath,
		DeviceAuthURL:          p.DeviceAuthURL,
		DeviceTokenURL:         p.DeviceTokenURL,
		DeviceVerifyURL:        p.DeviceVerifyURL,
		DeviceExchangeRedirect: p.DeviceExchangeRedirect,
		ClientID:               p.ClientID,
		Scopes:                 p.Scopes,
		RefreshSkewSecs:        p.RefreshSkewSecs,
		ExtraHeaders:           p.ExtraHeaders,
	}
}

// RuleCfg defines a conditional transform pipeline with matching criteria.
type RuleCfg struct {
	ID                 string         `yaml:"id"`
	Name               string         `yaml:"name"`
	PatternPath        string         `yaml:"pattern_path"`
	PatternModel       string         `yaml:"pattern_model,omitempty"`
	PatternModels      []string       `yaml:"pattern_models,omitempty"`
	MatchFormat        []string       `yaml:"match_format,omitempty"`
	MatchDownstreamFmt []string       `yaml:"match_downstream_format,omitempty"`
	MatchDownstreams   []string       `yaml:"match_downstreams,omitempty"`
	PipelineConfig     []PipelineStep `yaml:"pipeline_config,omitempty"`
	IsEnabled          bool           `yaml:"is_enabled"`
}

// PipelineStep is one transformer in a rule's pipeline.
type PipelineStep struct {
	PluginID string                 `json:"plugin_id" yaml:"plugin_id"`
	Config   map[string]interface{} `json:"config,omitempty" yaml:"config,omitempty"`
}

// AliasGroupCfg defines an alias group: one input model mapped to a list of
// output model options. The first option in the list is the active one.
// "is_active" is no longer stored in YAML — it is managed by the DB.
// If IsRegex is true, input_model_id is treated as a regular expression pattern.
// AnnouncedNames are concrete model IDs surfaced by regex groups in /v1/models;
// each entry must match InputModelID and must not collide with any existing
// downstream output_model_id, non-regex alias input_model_id, or another
// regex group's input_model_id or announced_name.
type AliasGroupCfg struct {
	InputModelID   string           `yaml:"input_model_id"`
	IsRegex        bool             `yaml:"is_regex,omitempty"`
	Options        []AliasOptionCfg `yaml:"options"`
	AnnouncedNames []string         `yaml:"announced_names,omitempty"`
}

// AliasOptionCfg defines a single output-model option within an alias group.
type AliasOptionCfg struct {
	ID            string `yaml:"id"`
	DownstreamID  string `yaml:"downstream_id"`
	OutputModelID string `yaml:"output_model_id"`
}

// Load reads the YAML config file. If configPath is empty, it auto-detects:
// first tries ./config.yaml, then $HOME/.tresor.yaml.
func Load(configPath string) (*AppConfig, error) {
	resolved := resolveConfigPath(configPath)
	if resolved == "" {
		home, _ := os.UserHomeDir()
		fallback := filepath.Join(home, ".tresor.yaml")
		return nil, fmt.Errorf("no config file found (tried: ./config.yaml, %s)", fallback)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", resolved, err)
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", resolved, err)
	}

	// yaml.v3 does not decode into unexported fields, so the legacy one-shot
	// back-compat fields (top-level api_key on a downstream, and the old
	// top-level oauth_providers section) must be read through a separate raw
	// struct whose fields are exported. The legacy values are aligned to the
	// primary config by slice index (same order in the YAML).
	var legacyRaw struct {
		Downstreams    []struct {
			LegacyAPIKey string `yaml:"api_key"`
		} `yaml:"downstreams"`
		OAuthProviders []OAuthProviderCfg `yaml:"oauth_providers"`
	}
	if err := yaml.Unmarshal(data, &legacyRaw); err != nil {
		return nil, fmt.Errorf("parse config file %s (legacy fields): %w", resolved, err)
	}
	if len(legacyRaw.Downstreams) != len(cfg.Downstreams) {
		// Defensive: the two parses must agree on downstream count. If they
		// don't (e.g. a future schema change), fail the legacy fold safely
		// rather than misaligning keys.
		legacyRaw.Downstreams = nil
	}
	for i, lr := range legacyRaw.Downstreams {
		if i < len(cfg.Downstreams) {
			cfg.Downstreams[i].legacyAPIKey = lr.LegacyAPIKey
		}
	}
	if len(legacyRaw.OAuthProviders) > 0 {
		cfg.legacyOAuthProviders = legacyRaw.OAuthProviders
	}

	// Apply defaults for required fields
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1:11510"
	}
	if cfg.ProxyMode == "" {
		cfg.ProxyMode = "auto"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath()
	} else {
		cfg.DBPath = expandTilde(cfg.DBPath)
	}

	// Expand tildes in paths
	if cfg.SocketPath != "" {
		cfg.SocketPath = expandTilde(cfg.SocketPath)
	}
	if cfg.IconCacheDir != "" {
		cfg.IconCacheDir = expandTilde(cfg.IconCacheDir)
	}

	// Initialize empty slices to avoid nil
	if cfg.Downstreams == nil {
		cfg.Downstreams = []DownstreamCfg{}
	}
	if cfg.Rules == nil {
		cfg.Rules = []RuleCfg{}
	}
	if cfg.Aliases == nil {
		cfg.Aliases = []AliasGroupCfg{}
	}
	if cfg.ProxyAPIKeys == nil {
		cfg.ProxyAPIKeys = []string{}
	}
	if cfg.legacyOAuthProviders == nil {
		cfg.legacyOAuthProviders = []OAuthProviderCfg{}
	}

	// Sanitize legacy ApiFormat -> ApiFormats and synthesize a per-downstream
	// Auth block from the legacy top-level api_key field (one-shot back-compat).
	for i := range cfg.Downstreams {
		ds := &cfg.Downstreams[i]
		if ds.ApiFormat != "" && len(ds.ApiFormats) == 0 {
			ds.ApiFormats = []string{ds.ApiFormat}
			ds.ApiFormat = ""
		}
		if ds.Auth == nil {
			// No explicit auth block: default to api_key, carrying over any
			// legacy top-level api_key value.
			ds.Auth = &DownstreamAuthCfg{Type: "api_key", APIKey: ds.legacyAPIKey}
		}
		ds.legacyAPIKey = ""
		if ds.Auth.Type == "" {
			ds.Auth.Type = "api_key"
		}
	}

	// Sanitize legacy PatternModel -> PatternModels for backward compatibility.
	// If a rule has a single pattern_model but no pattern_models list, promote
	// the scalar to a one-element list. When both are present, pattern_models
	// takes precedence (the user is already using the new format).
	for i := range cfg.Rules {
		if cfg.Rules[i].PatternModel != "" && len(cfg.Rules[i].PatternModels) == 0 {
			cfg.Rules[i].PatternModels = []string{cfg.Rules[i].PatternModel}
		}
		cfg.Rules[i].PatternModel = ""
	}

	// Store the resolved config path for write-back
	cfg.ConfigPath = resolved

	return &cfg, nil
}

// resolveConfigPath returns the path to use for config loading.
// Priority: explicit path > ./config.yaml > $HOME/.tresor.yaml
func resolveConfigPath(configPath string) string {
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			return configPath
		}
	}

	// Try ./config.yaml in current directory
	candidates := []string{"./config.yaml"}
	home, err := os.UserHomeDir()
	if err == nil {
		candidates = append(candidates, filepath.Join(home, ".tresor.yaml"))
	}

	for _, c := range candidates {
		expanded := expandTilde(c)
		if _, err := os.Stat(expanded); err == nil {
			return expanded
		}
	}

	return ""
}

// expandTilde replaces leading ~ with the user's home directory.
func expandTilde(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./tresor.db"
	}
	return filepath.Join(home, ".tresor.db")
}
