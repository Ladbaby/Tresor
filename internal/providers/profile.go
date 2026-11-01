// Package providers installs provider definitions ("profiles") into the store at
// runtime, so adding a provider never requires recompiling Tresor.
//
// A downstream in Tresor is pure declarative data (base_url, api_formats,
// format_urls, format_paths, auth, output model ids), and the engine already
// performs all the generic work. A profile is therefore data describing one
// provider, plus the optional JS plugin bindings that a provider needs for
// vendor-specific request shaping.
//
// A profile never carries credentials. This is what makes publishing profiles in
// a public catalog safe: an OAuth block (client id, endpoints, scopes) is
// publishable, its secret is not, and the user supplies secrets at install time.
package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"net/url"
	"tresor/internal/config"
)

// SchemaName identifies the profile format this package reads.
const SchemaName = "tresor-provider-v1"

// Profile is one provider definition.
type Profile struct {
	Schema   string            `json:"schema"`
	ID       string            `json:"id"`
	Version  int               `json:"version"`
	Name     string            `json:"name"`
	Icon     string            `json:"icon,omitempty"`
	Summary  map[string]string `json:"summary,omitempty"`
	Provider ProviderSpec      `json:"provider"`
	Plugins  []PluginBinding   `json:"plugins,omitempty"`
	Aliases  []AliasSpec       `json:"aliases,omitempty"`
	Requires Requires          `json:"requires,omitempty"`
}

// ProviderSpec is the downstream a profile creates. It deliberately mirrors the
// subset of config.DownstreamCfg that a provider can meaningfully declare.
type ProviderSpec struct {
	BaseURL     string            `json:"base_url"`
	ApiFormats  []string          `json:"api_formats,omitempty"`
	FormatURLs  map[string]string `json:"format_urls,omitempty"`
	FormatPaths map[string]string `json:"format_paths,omitempty"`
	Auth        *AuthSpec         `json:"auth,omitempty"`
	Models      []string          `json:"models,omitempty"`
}

// AuthSpec is the publishable part of a downstream's auth: the method and, for
// OAuth, the public client identity and endpoints. It has no secret fields at
// all, so a profile cannot express one.
type AuthSpec struct {
	Type     string `json:"type"` // "api_key" (default) | "oauth"
	Flow     string `json:"flow,omitempty"`
	ClientID string `json:"client_id,omitempty"`

	AuthorizationURL string `json:"authorization_url,omitempty"`
	TokenURL         string `json:"token_url,omitempty"`
	DiscoveryURL     string `json:"discovery_url,omitempty"`
	RedirectPort     int    `json:"redirect_port,omitempty"`
	RedirectPath     string `json:"redirect_path,omitempty"`

	DeviceAuthURL          string `json:"device_auth_url,omitempty"`
	DeviceTokenURL         string `json:"device_token_url,omitempty"`
	DeviceVerifyURL        string `json:"device_verify_url,omitempty"`
	DeviceExchangeRedirect string `json:"device_redirect_uri,omitempty"`

	Scopes               string                        `json:"scopes,omitempty"`
	ExtraAuthParams      map[string]string             `json:"extra_authorize_params,omitempty"`
	ExtraHeaders         map[string]config.HeaderValue `json:"extra_headers,omitempty"`
	Wire                 string                        `json:"wire,omitempty"`
	AllowedEndpointHosts []string                      `json:"allowed_endpoint_hosts,omitempty"`
}

// PluginBinding names a JS plugin package that this provider needs, and the
// configuration it runs with. Installing the package itself still requires the
// user's explicit consent, because that is code rather than data.
type PluginBinding struct {
	ID      string         `json:"id"`
	Version string         `json:"version,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
	// URL is where the package bundle is downloaded from when the profile is
	// installed with plugin consent. It is what makes one-click setup possible:
	// the recipe carries the code, not just the configuration for it.
	URL string `json:"url,omitempty"`
	// SHA256 pins the bundle's source digest, so a changed package at that URL is
	// rejected instead of silently installed.
	SHA256 string `json:"sha256,omitempty"`
}

// AliasSpec is a suggested model mapping. Install applies it only when no alias
// group already uses InputModelID, so installing a provider never rewrites
// routing the user set up.
type AliasSpec struct {
	InputModelID  string `json:"input_model_id"`
	OutputModelID string `json:"output_model_id"`
	IsRegex       bool   `json:"is_regex,omitempty"`
}

// Requires declares what the daemon must provide for the profile to work. It is
// what turns "this build is too old" into a specific, actionable message instead
// of a cryptic feature-rejection error at install time.
type Requires struct {
	Features   []string `json:"features,omitempty"`
	MinVersion string   `json:"min_version,omitempty"`
}

// Check reports why this daemon cannot run the profile, or nil when it can. It is
// what the catalog index's mirror of these fields is for, so a profile installed
// directly (inline, or with an index that omits the fields) is held to the same
// bar instead of failing later inside the plugin runtime.
func (r Requires) Check(have []string, version string) error {
	var missing []string
	for _, want := range r.Features {
		if !containsFold(have, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("requires %s, which this Tresor build does not provide; update Tresor to install it",
			strings.Join(missing, ", "))
	}
	if r.MinVersion != "" {
		if _, ok := parseVersion(version); ok && !versionAtLeast(version, r.MinVersion) {
			return fmt.Errorf("requires Tresor %s or newer (this build is %s)", r.MinVersion, version)
		}
	}
	return nil
}

// forbiddenKeys are field names that only ever carry a credential. A profile
// containing any of them is rejected wherever it appears, so a published
// catalog entry cannot smuggle a shared secret into users' installs.
var forbiddenKeys = map[string]string{
	"api_key":       "api_key",
	"apikey":        "apikey",
	"client_secret": "client_secret",
	"secret":        "secret",
	"password":      "password",
	"token":         "token",
	"access_token":  "access_token",
	"refresh_token": "refresh_token",
	"private_key":   "private_key",
}

// Parse validates and decodes a profile. Unknown fields are rejected so a typo
// or an unsupported future field fails loudly at install rather than silently
// doing nothing, and any credential-shaped field is refused outright.
func Parse(data []byte) (*Profile, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("provider profile is empty")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("provider profile is not valid JSON: %w", err)
	}
	if key, ok := findForbiddenKey(raw); ok {
		return nil, fmt.Errorf("provider profile must not contain credentials (found %q); supply credentials at install time instead", key)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("provider profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// findForbiddenKey walks decoded JSON for any credential-shaped key.
func findForbiddenKey(v any) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic error message
		for _, k := range keys {
			if _, bad := forbiddenKeys[strings.ToLower(k)]; bad {
				return k, true
			}
			if found, ok := findForbiddenKey(t[k]); ok {
				return found, true
			}
		}
	case []any:
		for _, item := range t {
			if found, ok := findForbiddenKey(item); ok {
				return found, true
			}
		}
	}
	return "", false
}

// Validate checks the profile's structure and defaults.
func (p *Profile) Validate() error {
	if p.Schema != SchemaName {
		return fmt.Errorf("unsupported profile schema %q (want %q)", p.Schema, SchemaName)
	}
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("provider profile: id is required")
	}
	if strings.ContainsAny(p.ID, " \t\r\n/") {
		return fmt.Errorf("provider profile: id %q must not contain whitespace or '/'", p.ID)
	}
	if p.Version < 1 {
		return fmt.Errorf("provider profile %s: version must be at least 1", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("provider profile %s: name is required", p.ID)
	}
	if strings.TrimSpace(p.Provider.BaseURL) == "" {
		return fmt.Errorf("provider profile %s: provider.base_url is required", p.ID)
	}
	// Normalize/default the auth block so the stored value is always shaped.
	if p.Provider.Auth == nil {
		p.Provider.Auth = &AuthSpec{Type: "api_key"}
	}
	if p.Provider.Auth.Type == "" {
		p.Provider.Auth.Type = "api_key"
	}
	switch p.Provider.Auth.Type {
	case "api_key", "oauth":
	default:
		return fmt.Errorf("provider profile %s: auth.type %q must be api_key or oauth", p.ID, p.Provider.Auth.Type)
	}
	// Declared headers are validated for every auth type, so a profile cannot
	// install a downstream that fails at request time.
	if err := config.ValidateHeaderValues(p.Provider.Auth.ExtraHeaders); err != nil {
		return fmt.Errorf("provider profile %s: %w", p.ID, err)
	}
	// A dialect name is only checked for syntax here, not availability: a recipe
	// that carries its own dialect package names a dialect that does not exist
	// yet, and validating availability up front would make such a recipe
	// uninstallable. The installer re-checks after installing the packages.
	if !ValidWireName(p.Provider.Auth.Wire) {
		return fmt.Errorf("provider profile %s: auth.wire %q must be a dialect name "+
			"(letters, digits, '-', '_', '.')", p.ID, p.Provider.Auth.Wire)
	}
	if p.Provider.Auth.Type == "oauth" {
		a := p.Provider.Auth
		switch a.Flow {
		case "auth_code":
			if a.DiscoveryURL == "" && (a.AuthorizationURL == "" || a.TokenURL == "") {
				return fmt.Errorf("provider profile %s: auth_code flow needs discovery_url or both authorization_url and token_url", p.ID)
			}
		case "device":
			if a.DeviceAuthURL == "" || a.DeviceTokenURL == "" {
				return fmt.Errorf("provider profile %s: device flow needs device_auth_url and device_token_url", p.ID)
			}
		default:
			return fmt.Errorf("provider profile %s: oauth flow %q must be auth_code or device", p.ID, a.Flow)
		}
	}
	seenPlugins := map[string]bool{}
	for i, pl := range p.Plugins {
		if strings.TrimSpace(pl.ID) == "" {
			return fmt.Errorf("provider profile %s: plugins[%d].id is required", p.ID, i)
		}
		if seenPlugins[pl.ID] {
			return fmt.Errorf("provider profile %s: plugin %q is listed twice", p.ID, pl.ID)
		}
		seenPlugins[pl.ID] = true
		// A binding that carries code must download over HTTPS, and a declared
		// digest must look like one — otherwise a published profile could point
		// the installer anywhere.
		if pl.URL != "" {
			parsed, err := url.Parse(pl.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
				return fmt.Errorf("provider profile %s: plugins[%d].url must be a public HTTPS URL", p.ID, i)
			}
		}
		if pl.SHA256 != "" && !isDigestHex(pl.SHA256) {
			return fmt.Errorf("provider profile %s: plugins[%d].sha256 must be a lowercase hex sha256 digest", p.ID, i)
		}
	}
	seenAliases := map[string]bool{}
	for i, a := range p.Aliases {
		if strings.TrimSpace(a.InputModelID) == "" || strings.TrimSpace(a.OutputModelID) == "" {
			return fmt.Errorf("provider profile %s: aliases[%d] needs input_model_id and output_model_id", p.ID, i)
		}
		if seenAliases[a.InputModelID] {
			return fmt.Errorf("provider profile %s: alias input %q is listed twice", p.ID, a.InputModelID)
		}
		seenAliases[a.InputModelID] = true
	}
	return nil
}

// Digest is the canonical digest of the profile. It covers the parsed, validated
// form rather than the original bytes, so formatting and key order in the
// published file do not change it. Credentials cannot be involved: Validate
// rejects them before a digest is ever computed.
func (p *Profile) Digest() (string, error) {
	canonical, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("canonicalize provider profile %s: %w", p.ID, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// AuthConfig converts the publishable auth spec into the store's auth config.
// The returned value carries no credentials; the caller merges in whatever the
// user supplied.
func (a *AuthSpec) AuthConfig() *config.DownstreamAuthCfg {
	if a == nil {
		return &config.DownstreamAuthCfg{Type: "api_key"}
	}
	cfg := &config.DownstreamAuthCfg{
		Type:                   a.Type,
		Flow:                   a.Flow,
		ClientID:               a.ClientID,
		AuthorizationURL:       a.AuthorizationURL,
		TokenURL:               a.TokenURL,
		DiscoveryURL:           a.DiscoveryURL,
		RedirectPort:           a.RedirectPort,
		RedirectPath:           a.RedirectPath,
		DeviceAuthURL:          a.DeviceAuthURL,
		DeviceTokenURL:         a.DeviceTokenURL,
		DeviceVerifyURL:        a.DeviceVerifyURL,
		DeviceExchangeRedirect: a.DeviceExchangeRedirect,
		Scopes:                 a.Scopes,
		ExtraAuthParams:        a.ExtraAuthParams,
		ExtraHeaders:           a.ExtraHeaders,
		Wire:                   a.Wire,
		AllowedEndpointHosts:   a.AllowedEndpointHosts,
	}
	if cfg.Type == "" {
		cfg.Type = "api_key"
	}
	return cfg
}

// isDigestHex reports whether s is a lowercase hex sha256 digest. The installer
// compares it to the downloaded bundle's source digest, so a malformed value must
// be rejected at parse time rather than silently rarely matching.
func isDigestHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidWireName reports whether name is a syntactically valid dialect name. An
// empty name means "no dialect", which is the default for a plain provider.
func ValidWireName(name string) bool {
	if name == "" {
		return true
	}
	if len(name) > 64 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
