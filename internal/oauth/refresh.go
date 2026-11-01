package oauth

import (
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
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
	var p *Provider
	if cached, ok, cacheErr := oauthCachedProvider(*auth, t.Extra); cacheErr != nil {
		return "", nil, cacheErr
	} else if ok {
		p = cached
	} else {
		p, err = m.buildProvider(downstreamID, auth)
		if err != nil {
			return "", nil, err
		}
	}
	p.Name = downstreamID

	// Token-level headers: values stored on the token row's extra map are passed
	// through as headers keyed by their extra-map key. They form the operator
	// override layer, so they win over a declarative value of the same name.
	tokenHeaders := map[string]string{}
	for k, v := range t.Extra {
		if s, ok := v.(string); ok && k != "id_token" && k != oauthEndpointsKey {
			tokenHeaders[k] = s
		}
	}

	if t.NeedsLogin {
		return "", nil, ErrNotConnected
	}

	// Fast path: still valid
	if exp := tokenExpiry(t.AccessToken, t.ExpiresAt); (exp.IsZero() && !p.RefreshOnUnknownExpiry) || (!exp.IsZero() && time.Now().Add(time.Duration(p.skew())*time.Second).Before(exp)) {
		return t.AccessToken, finalizeOAuthHeaders(tokenHeaders, t.AccessToken, p), nil
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
		if exp := tokenExpiry(t2.AccessToken, t2.ExpiresAt); (exp.IsZero() && !p.RefreshOnUnknownExpiry) || (!exp.IsZero() && time.Now().Add(time.Duration(p.skew())*time.Second).Before(exp)) {
			return t2.AccessToken, finalizeOAuthHeaders(tokenHeaders, t2.AccessToken, p), nil
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
		// Distinguish a definitive auth rejection from a transient failure.
		// Only a definitive failure (the refresh token is actually dead —
		// invalid_grant, access_denied, revoked session, ...) warrants marking
		// the downstream as needing a fresh interactive login. A transient
		// failure — network down, DNS, timeout, a 5xx from the token endpoint
		// — must NOT wedge the connection: leave the stored credential intact
		// and let the next request retry the refresh once the network recovers.
		// This mirrors the reference gateways' non-retryable-error classifier.
		if isDefinitiveRefreshError(err) {
			if markErr := m.store.SetOAuthNeedsLogin(downstreamID, true); markErr != nil {
				log.Printf("oauth: mark needs_login for %s: %v", downstreamID, markErr)
			}
			return "", nil, fmt.Errorf("refresh failed: %w (reconnect in the dashboard)", err)
		}
		// Transient: surface a retryable error and keep the credential so the
		// next request retries. Do NOT mark needs_login. Wrapping ErrTransientRefresh
		// lets callers tell this apart from a permanent failure.
		log.Printf("oauth: transient refresh failure for %s (will retry): %v", downstreamID, err)
		return "", nil, fmt.Errorf("%w: %v", ErrTransientRefresh, err)
	}

	nt := m.buildTokenRow(downstreamID, p, tr)
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken // provider omitted a rotated refresh token
	}
	// Preserve provider-specific extra fields (e.g. account_id, org UUID)
	// that the refresh response does not carry.
	if t.Extra != nil {
		for k, v := range t.Extra {
			if k == oauthEndpointsKey {
				continue
			}
			if _, exists := nt.Extra[k]; !exists {
				nt.Extra[k] = v
			}
		}
	}
	if err := m.store.SaveOAuthToken(nt); err != nil {
		return "", nil, err
	}
	return nt.AccessToken, finalizeOAuthHeaders(tokenHeaders, nt.AccessToken, p), nil
}

// finalizeOAuthHeaders resolves a provider's declarative extra headers against
// the FINAL access token about to be sent, then layers the operator's own
// token-row headers on top.
//
// Resolution is declarative (see headers.go): a literal is sent as-is, a
// from_claim value is copied out of the token's claims, and a template is filled
// from the closed host variable set. Provider identity is therefore data, not a
// branch in Go — a new provider's headers need no code here.
//
// It must run against the final token, not the pre-refresh one, because a refresh
// can rotate in a token whose claims differ (a different account id, say).
func finalizeOAuthHeaders(operatorHeaders map[string]string, accessToken string, p *Provider) map[string]string {
	resolved, err := ResolveHeaderValues(p.ExtraHeaders, accessToken, operatorHeaders)
	if err != nil {
		// A malformed declaration must not silently drop provider identity: fall
		// back to the operator headers so the request still carries what the
		// operator asked for, and report the problem.
		log.Printf("oauth: %s: refusing declared extra_headers: %v", p.Name, err)
		resolved = operatorHeaders
	}
	return resolved
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

// definitiveRefreshErrors are error substrings (lowercased) that indicate the
// refresh token itself is dead, not a transient problem. When one of these
// appears, the connection genuinely needs a fresh interactive login. This list
// mirrors the reference gateway (sub2api's isNonRetryableRefreshError) so a
// transient network blip is never mistaken for a revoked token.
var definitiveRefreshErrors = []string{
	"invalid_grant",
	"invalid_refresh_token",
	"refresh_token_reused",
	"refresh_token_invalidated",
	"token_expired",
	"access_denied",
	"invalid_client",
	"unauthorized_client",
	"app_session_terminated",
	"subscription required",
	"entitlement_denied",
}

// isDefinitiveRefreshError reports whether a refresh failure means the refresh
// token is unusable (mark needs_login) as opposed to a transient network /
// server hiccup (leave the credential and retry on the next request).
//
// A network error (dial/timeout/DNS) never contains any of these substrings,
// so it classifies as transient. A 400 whose body carries "invalid_grant" (now
// included in the error by postToken) classifies as definitive.
func isDefinitiveRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range definitiveRefreshErrors {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
