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
	BindAddr      string          `yaml:"bind_addr"`
	SocketPath    string          `yaml:"socket_path,omitempty"`
	DBPath        string          `yaml:"db_path"`
	AdminPassword string          `yaml:"admin_password,omitempty"`
	JWTSecret     []byte          `yaml:"-"` // derived from AdminPassword, not serialized

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

	// Routing data (loaded into SQLite at startup via upsert)
	Downstreams []DownstreamCfg   `yaml:"downstreams"`
	Rules       []RuleCfg         `yaml:"rules"`
	Aliases     []AliasGroupCfg   `yaml:"aliases"`

	// ConfigPath is the resolved file path of the YAML config.
	// Not serialized to YAML; used for write-back on mutations.
	ConfigPath string `yaml:"-"`
}

// DownstreamCfg defines a downstream LLM provider endpoint.
type DownstreamCfg struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	BaseURL        string   `yaml:"base_url"`
	APIKey         string   `yaml:"api_key,omitempty"`
	ApiFormats     []string `yaml:"api_formats,omitempty"`
	OutputModelIDs []string `yaml:"output_model_ids,omitempty"`

	// ApiFormat is a legacy field accepted for backward-compatible YAML loading.
	// It gets converted to ApiFormats during the sanitize step.
	ApiFormat string `yaml:"api_format,omitempty"`
}

// RuleCfg defines a conditional transform pipeline with matching criteria.
type RuleCfg struct {
	ID                  string         `yaml:"id"`
	Name                string         `yaml:"name"`
	PatternPath         string         `yaml:"pattern_path"`
	PatternModel        string         `yaml:"pattern_model,omitempty"`
	MatchFormat         []string       `yaml:"match_format,omitempty"`
	MatchDownstreamFmt  []string       `yaml:"match_downstream_format,omitempty"`
	MatchDownstreams    []string       `yaml:"match_downstreams,omitempty"`
	PipelineConfig      []PipelineStep `yaml:"pipeline_config,omitempty"`
	IsEnabled           bool           `yaml:"is_enabled"`
}

// PipelineStep is one transformer in a rule's pipeline.
type PipelineStep struct {
	PluginID string                 `json:"plugin_id" yaml:"plugin_id"`
	Config   map[string]interface{} `json:"config,omitempty" yaml:"config,omitempty"`
}

// AliasGroupCfg defines an alias group: one input model mapped to a list of
// output model options. The first option in the list is the active one.
// "is_active" is no longer stored in YAML — it is managed by the DB.
type AliasGroupCfg struct {
	InputModelID  string             `yaml:"input_model_id"`
	Options       []AliasOptionCfg   `yaml:"options"`
}

// AliasOptionCfg defines a single output-model option within an alias group.
type AliasOptionCfg struct {
	ID            string `yaml:"id"`
	DownstreamID  string `yaml:"downstream_id"`
	OutputModelID string `yaml:"output_model_id"`
	IsRegex       bool   `yaml:"is_regex,omitempty"`
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

	// Derive JWT secret from admin password
	if cfg.AdminPassword != "" {
		cfg.JWTSecret = []byte(cfg.AdminPassword)
	}

	// Expand tildes in paths
	if cfg.SocketPath != "" {
		cfg.SocketPath = expandTilde(cfg.SocketPath)
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

	// Sanitize legacy ApiFormat -> ApiFormats for backward compatibility
	for i := range cfg.Downstreams {
		if cfg.Downstreams[i].ApiFormat != "" && len(cfg.Downstreams[i].ApiFormats) == 0 {
			cfg.Downstreams[i].ApiFormats = []string{cfg.Downstreams[i].ApiFormat}
			cfg.Downstreams[i].ApiFormat = ""
		}
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
