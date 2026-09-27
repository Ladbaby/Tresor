package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tresor/internal/config"
)

// Flow types
const (
	FlowAuthCode = "auth_code" // Authorization Code + PKCE (loopback redirect)
	FlowDevice   = "device"    // RFC 8628 Device Code flow
)

const (
	defaultRedirectPort   = 56120
	defaultRedirectPath   = "/callback"
	defaultRefreshSkew    = 120 // seconds ahead of expiry to refresh
	devicePollTimeout     = 15 * time.Minute
	deviceMinPollInterval = 5 * time.Second
)

// Provider is a validated, ready-to-use OAuth provider definition.
type Provider struct {
	Name string
	Flow string

	// auth_code
	AuthorizationURL string
	TokenURL         string
	ClientID         string
	ClientSecret     string // confidential clients: sent as Basic auth on token requests
	Scopes           string
	RedirectPort     int
	RedirectPath     string
	// ExtraAuthParams are additional query params appended to the authorize
	// URL (e.g. Codex's codex_cli_simplified_flow).
	ExtraAuthParams map[string]string

	// device
	DeviceAuthURL          string
	DeviceTokenURL         string
	DeviceVerifyURL        string
	DeviceExchangeRedirect string

	RefreshSkewSecs int
	ExtraHeaders    map[string]string
}

// RedirectURI returns the loopback redirect URI for the auth_code flow.
func (p *Provider) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", p.RedirectPort, p.RedirectPath)
}

// tokenAuth returns the HTTP Basic auth credentials (user, pass) for token
// endpoint requests when this is a confidential client, or ("", "") for a
// public client.
func (p *Provider) tokenAuth() (string, string) {
	if p.ClientSecret != "" {
		return p.ClientID, p.ClientSecret
	}
	return "", ""
}

// Normalize validates a per-downstream auth config and fills defaults. When
// DiscoveryURL is set it is fetched to fill missing auth_code endpoints.
// cfg.Type must be "oauth" for this to be meaningful; the flow-specific
// required fields are enforced per cfg.Flow.
func Normalize(cfg config.DownstreamAuthCfg, httpClient *http.Client) (*Provider, error) {
	p := &Provider{
		Flow:                   cfg.Flow,
		AuthorizationURL:       cfg.AuthorizationURL,
		TokenURL:               cfg.TokenURL,
		DeviceAuthURL:          cfg.DeviceAuthURL,
		DeviceTokenURL:         cfg.DeviceTokenURL,
		DeviceVerifyURL:        cfg.DeviceVerifyURL,
		DeviceExchangeRedirect: cfg.DeviceExchangeRedirect,
		ClientID:               cfg.ClientID,
		ClientSecret:           cfg.ClientSecret,
		Scopes:                 cfg.Scopes,
		ExtraAuthParams:        cfg.ExtraAuthParams,
		RedirectPort:           cfg.RedirectPort,
		RedirectPath:           cfg.RedirectPath,
		RefreshSkewSecs:        cfg.RefreshSkewSecs,
		ExtraHeaders:           cfg.ExtraHeaders,
	}
	if p.RedirectPort == 0 {
		p.RedirectPort = defaultRedirectPort
	}
	if p.RedirectPath == "" {
		p.RedirectPath = defaultRedirectPath
	}
	if p.RefreshSkewSecs == 0 {
		p.RefreshSkewSecs = defaultRefreshSkew
	}

	if p.Flow == "" {
		return nil, fmt.Errorf("oauth auth: flow is required (\"auth_code\" or \"device\")")
	}

	switch p.Flow {
	case FlowAuthCode:
		// Optional OIDC discovery to fill endpoints
		if cfg.DiscoveryURL != "" && (p.AuthorizationURL == "" || p.TokenURL == "") {
			authURL, tokenURL, err := discoverEndpoints(cfg.DiscoveryURL, httpClient)
			if err != nil {
				return nil, fmt.Errorf("oauth auth: discovery: %w", err)
			}
			if p.AuthorizationURL == "" {
				p.AuthorizationURL = authURL
			}
			if p.TokenURL == "" {
				p.TokenURL = tokenURL
			}
		}
		if p.AuthorizationURL == "" || p.TokenURL == "" {
			return nil, fmt.Errorf("oauth auth: auth_code flow requires authorization_url and token_url (or discovery_url)")
		}
		for label, u := range map[string]string{"authorization_url": p.AuthorizationURL, "token_url": p.TokenURL} {
			if err := validateHTTPSURL(u, label); err != nil {
				return nil, fmt.Errorf("oauth auth: %w", err)
			}
		}
	case FlowDevice:
		if p.DeviceAuthURL == "" || p.DeviceTokenURL == "" || p.DeviceVerifyURL == "" {
			return nil, fmt.Errorf("oauth auth: device flow requires device_auth_url, device_token_url and device_verify_url")
		}
		for label, u := range map[string]string{"device_auth_url": p.DeviceAuthURL, "device_token_url": p.DeviceTokenURL} {
			if err := validateHTTPSURL(u, label); err != nil {
				return nil, fmt.Errorf("oauth auth: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("oauth auth: unknown flow %q (want \"auth_code\" or \"device\")", cfg.Flow)
	}
	return p, nil
}

// validateHTTPSURL ensures the URL parses and uses the https scheme.
// Loopback http URLs are allowed (local test servers).
func validateHTTPSURL(raw, label string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL: %w", label, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && u.Hostname() == "127.0.0.1" {
		return nil
	}
	return fmt.Errorf("%s must be an https URL", label)
}

// discoverEndpoints fetches an OIDC discovery document and returns the
// authorization and token endpoints.
func discoverEndpoints(discoveryURL string, httpClient *http.Client) (authURL, tokenURL string, err error) {
	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("discovery endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("parse discovery document: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return "", "", fmt.Errorf("discovery document missing authorization_endpoint or token_endpoint")
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, nil
}

// tokenResponse is the common shape of an OAuth2 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	// Device-flow specific: ChatGPT returns the PKCE verifier at the
	// device-poll stage for the subsequent code exchange.
	CodeVerifier string `json:"code_verifier"`
}

// parseTokenResponse decodes a token-endpoint body which may be either
// application/json or application/x-www-form-urlencoded.
func parseTokenResponse(body []byte, contentType string) (*tokenResponse, error) {
	var tr tokenResponse
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("parse token response: %w", err)
		}
		tr.AccessToken = vals.Get("access_token")
		tr.RefreshToken = vals.Get("refresh_token")
		tr.TokenType = vals.Get("token_type")
		if v := vals.Get("expires_in"); v != "" {
			fmt.Sscanf(v, "%d", &tr.ExpiresIn)
		}
	} else {
		if err := json.Unmarshal(body, &tr); err != nil {
			return nil, fmt.Errorf("parse token response: %w", err)
		}
	}
	if tr.AccessToken == "" {
		// Surface the provider's error message when present.
		var errBody struct {
			Error        string `json:"error"`
			ErrorMessage string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := strings.TrimSpace(errBody.Error + " " + errBody.ErrorMessage)
		if msg == "" {
			msg = "no access_token in response"
		}
		return nil, fmt.Errorf("token exchange failed: %s", msg)
	}
	if tr.TokenType == "" {
		tr.TokenType = "Bearer"
	}
	return &tr, nil
}
