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
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// Regions are the endpoint choices a vendor sells the same provider at —
	// a cloud's regions, a vendor's plans, a relay's national endpoints. One
	// recipe serves all of them, because the differences are only where the
	// requests go, and splitting them into separate recipes would duplicate the
	// auth shape, the model list and the plugin bindings per region while making
	// a drift between them invisible.
	//
	// A region inherits every field it omits from the provider, so a vendor whose
	// regions differ only in host writes one base_url per region and nothing else.
	// The provider's own base_url stays the default, which is what an install
	// without a region choice uses.
	Regions []Region `json:"regions,omitempty"`
}

// Region is one endpoint choice of a provider that offers several. Installing
// with a region writes that region's endpoints into the downstream, so a region
// is ordinary downstream data by the time it reaches the engine — the engine
// never learns that "regions" exist.
type Region struct {
	// ID names the region; it is what an install passes back to select it, so it
	// is a slug rather than a display string.
	ID string `json:"id"`
	// Name is the region as a user reads it ("US East (N. Virginia)"). Empty
	// means the ID doubles as the name.
	Name string `json:"name,omitempty"`
	// BaseURL replaces the provider's. Empty inherits it.
	BaseURL string `json:"base_url,omitempty"`
	// ApiFormats replaces the provider's format list when non-empty. A vendor's
	// plans do not always serve the same APIs — Volcengine Ark's pay as you go
	// answers no Anthropic endpoint although its Coding and Agent Plans do — and a
	// format declared but unroutable fails at request time instead of refusing the
	// request up front.
	ApiFormats []string `json:"api_formats,omitempty"`
	// FormatURLs are merged over the provider's: a format this region names wins
	// here, and one it maps to "" drops the provider's override so that format
	// falls back to this region's base_url.
	FormatURLs map[string]string `json:"format_urls,omitempty"`
	// FormatPaths are merged over the provider's, the same way.
	FormatPaths map[string]string `json:"format_paths,omitempty"`
	// Models replaces the provider's model list when non-empty. A cloud region or
	// a vendor's plan often serves a different set, and Python-style inheritance
	// of "the same models" would advertise ids the endpoint does not have.
	Models []string `json:"models,omitempty"`
	// UnknownModels declares that this region's model list is not known, so the
	// provider's list must not be inherited. Without it a region could only say
	// "different models" by listing some, so a host whose list is genuinely unknown
	// would either inherit a wrong list or force a stub id into the recipe. An
	// empty list after resolution means unknown, which is what Fetch Models is for.
	UnknownModels bool `json:"unknown_models,omitempty"`
}

// DisplayName is the region's label for a user: its name, or its id when it has
// none.
func (r Region) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// resolved returns the provider as it is at region r: every field the region
// omits inherited from the provider.
func (p *ProviderSpec) resolved(r Region) ProviderSpec {
	out := ProviderSpec{
		BaseURL:     cmp.Or(r.BaseURL, p.BaseURL),
		ApiFormats:  append([]string{}, p.ApiFormats...),
		FormatURLs:  mergeFormatMap(p.FormatURLs, r.FormatURLs),
		FormatPaths: mergeFormatMap(p.FormatPaths, r.FormatPaths),
		Auth:        p.Auth,
		Models:      append([]string{}, p.Models...),
	}
	if len(r.Models) > 0 {
		out.Models = append([]string{}, r.Models...)
	}
	if r.UnknownModels {
		// A region that cannot state a model list must not inherit one, or it would
		// advertise ids its endpoint does not serve. An empty list here means
		// "unknown", which the UI resolves with Fetch Models against that region.
		out.Models = []string{}
	}
	if len(r.ApiFormats) > 0 {
		out.ApiFormats = append([]string{}, r.ApiFormats...)
	}
	// A region that drops a format also drops that format's overrides. Leaving
	// them would show a URL in the UI for an API the provider no longer declares,
	// which reads as "this works" when the engine can never select it.
	pruneFormats(out.FormatURLs, out.ApiFormats)
	pruneFormats(out.FormatPaths, out.ApiFormats)
	out.Regions = nil
	if out.ApiFormats == nil {
		out.ApiFormats = []string{}
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	return out
}

// mergeFormatMap lays over on base. An empty overlay value removes the base's
// entry rather than setting an empty URL, because "" would otherwise be read as
// "no override" everywhere downstream and the base's URL would silently win —
// the opposite of what a region that clears it asked for.
func mergeFormatMap(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pruneFormats removes entries for formats the provider does not declare, so a
// map never names an API the engine cannot select.
func pruneFormats(m map[string]string, formats []string) {
	for k := range m {
		if !slices.Contains(formats, k) {
			delete(m, k)
		}
	}
}

// Region returns the region with that id.
func (p *ProviderSpec) Region(id string) (Region, bool) {
	for _, r := range p.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return Region{}, false
}

// DefaultRegion is the region an install that names none gets: the provider's
// own endpoints when it has a base_url, otherwise its first region.
func (p *ProviderSpec) DefaultRegion() string {
	if p.BaseURL != "" || len(p.Regions) == 0 {
		return ""
	}
	return p.Regions[0].ID
}

// AtRegion returns the provider as it is at regionID. An empty id means the
// provider's own endpoints. An unknown id is an error, not a silent fallback: a
// recipe that renamed its regions would otherwise install somewhere the user did
// not pick.
func (p *ProviderSpec) AtRegion(regionID string) (ProviderSpec, error) {
	if regionID == "" {
		if p.BaseURL != "" || len(p.Regions) == 0 {
			return p.resolved(Region{}), nil
		}
		return p.resolved(p.Regions[0]), nil
	}
	r, ok := p.Region(regionID)
	if !ok {
		return ProviderSpec{}, fmt.Errorf("no region %q (available: %s)", regionID, strings.Join(p.RegionIDs(), ", "))
	}
	return p.resolved(r), nil
}

// RegionIDs lists the region ids in the order the recipe declares them.
func (p *ProviderSpec) RegionIDs() []string {
	ids := make([]string, 0, len(p.Regions))
	for _, r := range p.Regions {
		ids = append(ids, r.ID)
	}
	return ids
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

	// OAuth policy. These decide how a token is obtained and renewed, and a
	// provider for whom they are wrong fails only at request time — long after the
	// install looked fine — so a profile that names OAuth must be able to state
	// them. All six are publishable: none holds a credential.
	RequestNonce               bool `json:"request_nonce,omitempty"`
	RequireRefreshToken        bool `json:"require_refresh_token,omitempty"`
	RefreshOnUnknownExpiry     bool `json:"refresh_on_unknown_expiry,omitempty"`
	RefreshSkewSecs            int  `json:"refresh_skew_secs,omitempty"`
	DefaultTokenLifetimeSecs   int  `json:"default_token_lifetime_secs,omitempty"`
	PersistDiscoveredEndpoints bool `json:"persist_discovered_endpoints,omitempty"`
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

// validateRegions checks the region list: ids are slugs and unique, and every
// region ends up with an endpoint of its own. A region that inherits an empty
// base_url would install a downstream pointing nowhere, which fails at request
// time rather than at install — the whole reason profiles are validated.
func (p *ProviderSpec) validateRegions() error {
	seen := map[string]bool{}
	for i, r := range p.Regions {
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("regions[%d].id is required", i)
		}
		for _, c := range r.ID {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
			if !ok {
				return fmt.Errorf("regions[%d].id %q must be a slug (letters, digits, '-', '_', '.')", i, r.ID)
			}
		}
		if seen[r.ID] {
			return fmt.Errorf("region %q is listed twice", r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.BaseURL) == "" && strings.TrimSpace(p.BaseURL) == "" {
			return errors.New("region " + r.ID + " needs a base_url: the provider declares none to inherit")
		}
		// Listing models and calling the list unknown contradict each other, and
		// either reading would be a guess about which one the author meant. Refusing
		// it costs an error while writing the recipe; guessing costs a published
		// recipe that installs the wrong model ids.
		if r.UnknownModels && len(r.Models) > 0 {
			return fmt.Errorf("region %q declares both models and unknown_models; keep one", r.ID)
		}
	}
	return nil
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
	// A provider must land on some endpoint: either its own base_url, or regions
	// to pick between. Requiring base_url alone would make a region-only recipe
	// impossible, and requiring regions alone would break every existing profile.
	if strings.TrimSpace(p.Provider.BaseURL) == "" && len(p.Provider.Regions) == 0 {
		return fmt.Errorf("provider profile %s: provider.base_url or at least one provider.regions entry is required", p.ID)
	}
	if err := p.Provider.validateRegions(); err != nil {
		return fmt.Errorf("provider profile %s: %w", p.ID, err)
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

		RequestNonce:               a.RequestNonce,
		RequireRefreshToken:        a.RequireRefreshToken,
		RefreshOnUnknownExpiry:     a.RefreshOnUnknownExpiry,
		RefreshSkewSecs:            a.RefreshSkewSecs,
		DefaultTokenLifetimeSecs:   a.DefaultTokenLifetimeSecs,
		PersistDiscoveredEndpoints: a.PersistDiscoveredEndpoints,
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
