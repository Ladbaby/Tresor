package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"tresor/internal/config"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

// ErrNotConnected indicates a downstream is marked as OAuth but has no
// usable token (never logged in, or the refresh token is exhausted).
var ErrNotConnected = errors.New("provider not connected")

// ErrTransientRefresh wraps a token-refresh failure that is transient (network
// down, DNS, timeout, 5xx) rather than a definitive rejection of the refresh
// token. Callers can use errors.Is to distinguish it from a permanent failure
// and respond with "try again" instead of "re-login": the stored credential is
// left intact and the next request will retry the refresh automatically.
var ErrTransientRefresh = errors.New("refresh temporarily unavailable")

// Manager coordinates OAuth logins, token storage and refresh for
// downstreams. Tokens are persisted via the store (SQLite). OAuth provider
// configuration is stored per-downstream in the downstreams.auth column, so
// there is no separate in-memory provider registry.
type Manager struct {
	store *store.Store

	clientMu sync.RWMutex
	client   *http.Client

	// callbackBase is the publicly reachable base URL of the daemon (e.g.
	// "http://127.0.0.1:11510"). The auth_code loopback redirect URI is
	// callbackBase + "/api/oauth/callback". When empty, falls back to the
	// downstream's own redirect port/path configuration.
	callbackBase string

	mu        sync.RWMutex

	pendingMu    sync.Mutex
	pending      map[string]*pendingLogin // keyed by state (auth_code) or downstreamID (device)
	pendingByKey map[string]*pendingLogin // downstreamID -> pending (any flow)

	refreshLocksMu sync.Mutex
	refreshLocks   map[string]*sync.Mutex
}

// pendingLogin tracks an in-flight interactive login.
type pendingLogin struct {
	DownstreamID string
	ProviderName string
	Flow         string
	Status       string // "pending" | "success" | "failed"
	Error        string
	StartedAt    time.Time

	// auth is a snapshot of the downstream's oauth config captured at
	// login start, used to build the provider during the callback/token
	// exchange without re-reading the store.
	auth *config.DownstreamAuthCfg

	// auth_code flow
	State        string
	CodeVerifier string

	// device flow
	UserCode       string
	DeviceCode     string
	DeviceAuthID   string
	AuthorizedCode string
	PollVerifier   string
	DevicePollURL  string // empty = standard RFC 8628 via TokenURL
	pollCancel     context.CancelFunc
}

func NewManager(s *store.Store, mode proxy.Mode) *Manager {
	return &Manager{
		store:        s,
		client:       buildClient(mode),
		pending:      map[string]*pendingLogin{},
		pendingByKey: map[string]*pendingLogin{},
		refreshLocks: map[string]*sync.Mutex{},
	}
}

// buildClient returns a proxy-aware HTTP client matching the engine's
// outbound transport (mirrors engine.SetProxyMode).
func buildClient(mode proxy.Mode) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               proxy.ProxyFunc(mode),
			IdleConnTimeout:     30 * time.Second,
			MaxIdleConns:        25,
			MaxIdleConnsPerHost: 5,
			DisableCompression:  true,
		},
		Timeout: 30 * time.Second,
	}
}

// SetProxyMode rebuilds the outbound client when the proxy mode changes at
// runtime.
func (m *Manager) SetProxyMode(mode proxy.Mode) {
	m.clientMu.Lock()
	m.client = buildClient(mode)
	m.clientMu.Unlock()
}

func (m *Manager) httpClient() *http.Client {
	m.clientMu.RLock()
	defer m.clientMu.RUnlock()
	return m.client
}

// SetCallbackBase sets the daemon base URL used to build the auth_code
// redirect URI. host is the bind address (e.g. "127.0.0.1:11510"); the
// scheme is always http since OAuth loopback redirects must not use TLS.
func (m *Manager) SetCallbackBase(host string) {
	m.mu.Lock()
	if host != "" {
		m.callbackBase = "http://" + host
	}
	m.mu.Unlock()
}

// redirectURIFor returns the redirect URI a provider's auth_code flow
// should use: the daemon callback when a callback base is configured,
// else the provider's own loopback configuration.
func (m *Manager) redirectURIFor(p *Provider) string {
	m.mu.RLock()
	base := m.callbackBase
	m.mu.RUnlock()
	if base != "" {
		return base + "/api/oauth/callback"
	}
	return p.RedirectURI()
}

// buildProvider constructs a validated Provider from a downstream's unified
// auth config. The Provider.Name is set to the downstream ID so token rows
// and pending logins can reference it unambiguously. Returns an error when
// the auth config is not a valid oauth definition.
func (m *Manager) buildProvider(dsID string, auth *config.DownstreamAuthCfg) (*Provider, error) {
	if auth == nil || auth.Type != "oauth" {
		return nil, fmt.Errorf("downstream %s is not configured for oauth", dsID)
	}
	p, err := Normalize(*auth, m.httpClient())
	if err != nil {
		return nil, err
	}
	p.Name = dsID
	return p, nil
}

// authFor loads a downstream's auth config from the store.
func (m *Manager) authFor(dsID string) (*config.DownstreamAuthCfg, error) {
	ds, err := m.store.GetDownstream(dsID)
	if err != nil {
		return nil, fmt.Errorf("downstream %s not found: %w", dsID, err)
	}
	return ds.Auth, nil
}

// StatusInfo is the login/connection state of one downstream.
type StatusInfo struct {
	Connected  bool      `json:"connected"`
	Status     string    `json:"status"` // "idle" | "pending" | "failed" | "connected"
	Error      string    `json:"error,omitempty"`
	NeedsLogin bool      `json:"needs_login"`
	HasToken   bool      `json:"has_token"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Flow       string    `json:"flow,omitempty"`
}

// Status reports the OAuth state for a downstream, combining the token row
// and any in-flight login.
func (m *Manager) Status(downstreamID string) (StatusInfo, error) {
	t, err := m.store.GetOAuthToken(downstreamID)
	if err != nil {
		return StatusInfo{}, err
	}
	info := StatusInfo{}
	pl := m.pendingFor(downstreamID)
	if pl != nil && pl.Status == "pending" {
		// Login is still in flight — report that, not the (cleared) token.
		info.Status = "pending"
		info.Provider = pl.ProviderName
		info.Flow = pl.Flow
		return info, nil
	}
	if t != nil {
		info.Provider = t.Provider
		info.Flow = t.Flow
		info.HasToken = true
		info.NeedsLogin = t.NeedsLogin
		if exp := tokenExpiry(t.AccessToken, t.ExpiresAt); !exp.IsZero() {
			info.ExpiresAt = exp
		}
		if t.NeedsLogin {
			info.Status = "failed"
			info.Error = "token expired and could not be refreshed — reconnect required"
		} else {
			info.Connected = true
			info.Status = "connected"
		}
	} else {
		info.Status = "idle"
		if pl != nil && pl.Status == "failed" {
			info.Status = "failed"
			info.Error = pl.Error
			info.Provider = pl.ProviderName
			info.Flow = pl.Flow
		}
	}
	return info, nil
}

func (m *Manager) pendingFor(downstreamID string) *pendingLogin {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return m.pendingByKey[downstreamID]
}

// StartLogin begins an interactive OAuth login for a downstream. It
// switches the downstream to OAuth mode immediately and returns the info
// the UI needs (authorize URL for auth_code, user code for device).
func (m *Manager) StartLogin(downstreamID string) (LoginInfo, error) {
	auth, err := m.authFor(downstreamID)
	if err != nil {
		return LoginInfo{}, err
	}
	p, err := m.buildProvider(downstreamID, auth)
	if err != nil {
		return LoginInfo{}, err
	}

	// Persist the oauth binding (idempotent — the UI has already set
	// auth.type=oauth with the recipe; this guards against a race) and clear
	// any stale token so a fresh login cannot silently reuse it.
	if err := m.store.SetDownstreamAuth(downstreamID, auth); err != nil {
		return LoginInfo{}, err
	}
	_ = m.store.DeleteOAuthToken(downstreamID)
	m.cancelPendingFor(downstreamID)

	pl := &pendingLogin{
		DownstreamID: downstreamID,
		ProviderName: downstreamID,
		Flow:         p.Flow,
		Status:       "pending",
		StartedAt:    time.Now(),
		auth:         auth,
	}
	if p.Flow == FlowAuthCode {
		verifier := make([]byte, 32)
		state := make([]byte, 32)
		if _, err := randRead(verifier); err != nil {
			return LoginInfo{}, err
		}
		if _, err := randRead(state); err != nil {
			return LoginInfo{}, err
		}
		pl.State = base64.RawURLEncoding.EncodeToString(state)
		pl.CodeVerifier = base64.RawURLEncoding.EncodeToString(verifier)
		challenge := sha256.Sum256([]byte(pl.CodeVerifier))
		challengeB64 := base64.RawURLEncoding.EncodeToString(challenge[:])
		q := url.Values{}
		q.Set("response_type", "code")
		if p.ClientID != "" {
			q.Set("client_id", p.ClientID)
		}
		q.Set("redirect_uri", m.redirectURIFor(p))
		if p.Scopes != "" {
			q.Set("scope", p.Scopes)
		}
		q.Set("code_challenge", challengeB64)
		q.Set("code_challenge_method", "S256")
		q.Set("state", pl.State)
		// Provider-specific extra params (e.g. Codex's codex_cli_simplified_flow).
		for k, v := range p.ExtraAuthParams {
			q.Set(k, v)
		}
		m.pendingMu.Lock()
		m.pending[pl.State] = pl
		m.pendingByKey[downstreamID] = pl
		m.pendingMu.Unlock()
		return LoginInfo{
			Status:       "pending",
			AuthorizeURL: p.AuthorizationURL + "?" + q.Encode(),
			RedirectURI:  m.redirectURIFor(p),
		}, nil
	}

	// Device flow
	_, err = m.requestDeviceLogin(p, pl)
	if err != nil {
		pl.Status = "failed"
		pl.Error = err.Error()
		m.pendingMu.Lock()
		m.pendingByKey[downstreamID] = pl
		m.pendingMu.Unlock()
		return LoginInfo{}, err
	}
	m.pendingMu.Lock()
	m.pendingByKey[downstreamID] = pl
	m.pendingMu.Unlock()
	return LoginInfo{
		Status:          "pending",
		VerificationURL: p.DeviceVerifyURL,
		UserCode:        pl.UserCode,
	}, nil
}

// LoginInfo is what the UI needs after StartLogin.
type LoginInfo struct {
	Status          string `json:"status"`
	AuthorizeURL    string `json:"authorize_url,omitempty"`
	RedirectURI     string `json:"redirect_uri,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
}

// CancelPending aborts an in-flight login for a downstream (e.g. user
// changed their mind before finishing).
func (m *Manager) CancelPending(downstreamID string) {
	m.cancelPendingFor(downstreamID)
}

func (m *Manager) cancelPendingFor(downstreamID string) {
	m.pendingMu.Lock()
	pl := m.pendingByKey[downstreamID]
	if pl != nil {
		delete(m.pendingByKey, downstreamID)
		if pl.State != "" {
			delete(m.pending, pl.State)
		}
	}
	m.pendingMu.Unlock()
	if pl != nil && pl.pollCancel != nil {
		pl.pollCancel()
	}
}

// Disconnect removes a downstream's OAuth connection and reverts it to
// API-key mode.
func (m *Manager) Disconnect(downstreamID string) error {
	m.cancelPendingFor(downstreamID)
	if err := m.store.DeleteOAuthToken(downstreamID); err != nil {
		return err
	}
	return m.store.SetDownstreamAuth(downstreamID, &config.DownstreamAuthCfg{Type: "api_key"})
}

// refreshLock returns (creating if needed) the per-downstream refresh mutex.
func (m *Manager) refreshLock(downstreamID string) *sync.Mutex {
	m.refreshLocksMu.Lock()
	defer m.refreshLocksMu.Unlock()
	l, ok := m.refreshLocks[downstreamID]
	if !ok {
		l = &sync.Mutex{}
		m.refreshLocks[downstreamID] = l
	}
	return l
}

// skew returns the refresh skew for a provider (default if
// the provider is unknown or unset). A skew of 0 means refresh only at/after
// the token's actual expiry (pi-style lazy refresh).
func (p *Provider) skew() int {
	if p.RefreshSkewSecs > 0 {
		return p.RefreshSkewSecs
	}
	return defaultRefreshSkew
}

// constantTimeEq compares two strings in constant time (state param check).
func constantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var _ = log.Printf // keep log import for device poller
