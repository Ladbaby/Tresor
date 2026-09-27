package oauth

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"tresor/internal/store"
)

// ResolveValidToken returns a currently-valid access token for a
// downstream's OAuth connection, refreshing it when close to expiry.
//
// Returns ErrNotConnected when the downstream has no token or the stored
// refresh token is exhausted — the engine should surface a
// "connect in the dashboard" error instead of forwarding.
//
// extraHeaders are the provider's static extra headers (e.g. originator).
func (m *Manager) ResolveValidToken(downstreamID string) (string, map[string]string, error) {
	t, err := m.store.GetOAuthToken(downstreamID)
	if err != nil {
		return "", nil, err
	}
	if t == nil {
		return "", nil, ErrNotConnected
	}
	auth, err := m.authFor(downstreamID)
	if err != nil {
		return "", nil, err
	}
	p, err := m.buildProvider(downstreamID, auth)
	if err != nil {
		return "", nil, err
	}
	extra := map[string]string{}
	for k, v := range p.ExtraHeaders {
		extra[k] = v
	}
	// Token-level extra headers: values stored on the token row's extra map
	// are passed through as headers keyed by their extra-map key.
	for k, v := range t.Extra {
		if s, ok := v.(string); ok && k != "id_token" {
			extra[k] = s
		}
	}

	if t.NeedsLogin {
		return "", nil, ErrNotConnected
	}

	// Fast path: still valid
	if exp := tokenExpiry(t.AccessToken, t.ExpiresAt); exp.IsZero() || time.Now().Add(time.Duration(p.skew())*time.Second).Before(exp) {
		return t.AccessToken, finalizeOAuthHeaders(extra, t.AccessToken, p), nil
	}

	// Needs a refresh
	lock := m.refreshLock(downstreamID)
	lock.Lock()
	defer lock.Unlock()

	// Double-check: a concurrent call may have already refreshed.
	t2, err := m.store.GetOAuthToken(downstreamID)
	if err != nil {
		return "", nil, err
	}
	if t2 != nil && !t2.NeedsLogin {
		if exp := tokenExpiry(t2.AccessToken, t2.ExpiresAt); exp.IsZero() || time.Now().Add(time.Duration(p.skew())*time.Second).Before(exp) {
			return t2.AccessToken, finalizeOAuthHeaders(extra, t2.AccessToken, p), nil
		}
		t = t2
	}

	if t.RefreshToken == "" {
		// Nothing to refresh with; the token is expired.
		_ = m.store.SetOAuthNeedsLogin(downstreamID, true)
		return "", nil, ErrNotConnected
	}

	tr, err := m.refreshToken(p, t)
	if err != nil {
		// Refresh failed: require a fresh interactive login.
		if markErr := m.store.SetOAuthNeedsLogin(downstreamID, true); markErr != nil {
			log.Printf("oauth: mark needs_login for %s: %v", downstreamID, markErr)
		}
		return "", nil, fmt.Errorf("refresh failed: %w (reconnect in the dashboard)", err)
	}

	nt := m.buildTokenRow(downstreamID, p, tr)
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken // provider omitted a rotated refresh token
	}
	// Preserve provider-specific extra fields (e.g. account_id, org UUID)
	// that the refresh response does not carry.
	if t.Extra != nil {
		for k, v := range t.Extra {
			if _, exists := nt.Extra[k]; !exists {
				nt.Extra[k] = v
			}
		}
	}
	if err := m.store.SaveOAuthToken(nt); err != nil {
		return "", nil, err
	}
	return nt.AccessToken, finalizeOAuthHeaders(extra, nt.AccessToken, p), nil
}

// finalizeOAuthHeaders layers provider identity headers onto the base extra
// headers, derived from the FINAL access token about to be sent. Two providers
// get special-cased so Tresor's subscription traffic fingerprints like the
// official CLI instead of an unknown client (the goal is to avoid the account
// being flagged):
//
//   - ChatGPT/Codex: when the access token carries the chatgpt_account_id
//     claim, send chatgpt-account-id, originator, and a pi User-Agent (matching
//     the pi / litellm reference clients).
//   - Grok (xAI): when the provider's scopes include "grok-cli:access" (the
//     xAI Grok CLI's client), send the grok-shell User-Agent and the
//     x-grok-client-identifier header (matching xai-org/grok-build).
//
// Operator-supplied extra_headers (already merged into base) always take
// precedence. It must run against the final token — not the pre-refresh one —
// because a refresh can rotate in a token whose account claim differs.
func finalizeOAuthHeaders(base map[string]string, accessToken string, p *Provider) map[string]string {
	out := make(map[string]string, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	// ChatGPT/Codex identity.
	if acct := jwtChatGPTAccountID(accessToken); acct != "" {
		if _, ok := out["chatgpt-account-id"]; !ok {
			out["chatgpt-account-id"] = acct
		}
		if _, ok := out["originator"]; !ok {
			out["originator"] = "pi"
		}
		if _, ok := out["User-Agent"]; !ok {
			out["User-Agent"] = codexUserAgent()
		}
	}
	// Grok (xAI) identity.
	if p != nil && isGrokProvider(p) {
		if _, ok := out["User-Agent"]; !ok {
			out["User-Agent"] = grokUserAgent()
		}
		if _, ok := out["x-grok-client-identifier"]; !ok {
			out["x-grok-client-identifier"] = "grok-shell"
		}
	}
	return out
}

// isGrokProvider reports whether an OAuth provider is xAI's Grok CLI client.
// The "grok-cli:access" scope is unique to the Grok CLI's registered client,
// so its presence is a reliable signal even if other fields are customized.
func isGrokProvider(p *Provider) bool {
	return strings.Contains(p.Scopes, "grok-cli:access")
}

// refreshToken performs a grant_type=refresh_token exchange.
func (m *Manager) refreshToken(p *Provider, t *store.OAuthToken) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", t.RefreshToken)
	if p.ClientID != "" {
		form.Set("client_id", p.ClientID)
	}
	return m.postToken(p, form)
}

// StartRefresher runs a background loop that proactively refreshes tokens
// before they expire. It stops when ctx is cancelled.
func (m *Manager) StartRefresher(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshDueNow()
			}
		}
	}()
}

// refreshDueNow refreshes every OAuth downstream whose token is within its
// provider's skew window of expiry. Exported-for-tests.
func (m *Manager) refreshDueNow() {
	ids, err := m.store.ListOAuthDownstreams()
	if err != nil {
		log.Printf("oauth: list downstreams: %v", err)
		return
	}
	now := time.Now()
	for _, id := range ids {
		t, err := m.store.GetOAuthToken(id)
		if err != nil || t == nil || t.NeedsLogin || t.RefreshToken == "" {
			continue
		}
		auth, err := m.authFor(id)
		if err != nil || auth == nil || auth.Type != "oauth" {
			continue
		}
		p, err := m.buildProvider(id, auth)
		if err != nil {
			continue
		}
		if !tokenExpiringSoon(t, p.skew(), now) {
			continue
		}
		if _, _, err := m.ResolveValidToken(id); err != nil {
			log.Printf("oauth: proactive refresh for %s: %v", id, err)
		}
	}
}

// codexUserAgent mirrors the reference client's User-Agent shape
// "pi (<os> <release>; <arch>)", e.g. "pi (linux 6.8.0; amd64)". The OS release
// is best-effort (it varies by platform and may be empty); the lookup is
// memoized because the header is built on every request.
var (
	codexUAOnce sync.Once
	codexUA     string
)

func codexUserAgent() string {
	codexUAOnce.Do(func() {
		codexUA = "pi (" + runtime.GOOS + " " + osRelease() + "; " + runtime.GOARCH + ")"
	})
	return codexUA
}

// osRelease returns the OS release string for the current platform, or "" if
// it can't be determined. Best-effort: a failure simply yields an empty release.
func osRelease() string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("uname", "-r")
	case "darwin":
		cmd = exec.Command("sw_vers", "-productVersion")
	case "windows":
		// release /10.0 corresponds to Windows 10/11; keep it coarse.
		return "10.0"
	default:
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// grokUserAgent returns the User-Agent shape the xAI Grok CLI sends,
// "grok-shell/<version> (<os>; <arch>)". The OS/arch use the Grok CLI's own
// naming (macos/aarch64). Memoized because it is built on every request.
const grokShellVersion = "1.0.0"

var (
	grokUAOnce sync.Once
	grokUA     string
)

func grokUserAgent() string {
	grokUAOnce.Do(func() {
		grokUA = "grok-shell/" + grokShellVersion + " (" + grokOS() + "; " + grokArch() + ")"
	})
	return grokUA
}

// grokOS maps the Go OS name to the Grok CLI's OS token.
func grokOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return runtime.GOOS
	}
}

// grokArch maps the Go arch to the Grok CLI's arch token (Rust-style aarch64).
func grokArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}
