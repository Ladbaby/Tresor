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
		return t.AccessToken, finalizeOAuthHeaders(extra, t.AccessToken), nil
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
			return t2.AccessToken, finalizeOAuthHeaders(extra, t2.AccessToken), nil
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
	return nt.AccessToken, finalizeOAuthHeaders(extra, nt.AccessToken), nil
}

// finalizeOAuthHeaders layers the ChatGPT/Codex identity headers onto a
// provider's base extra headers, derived from the FINAL access token about to
// be sent. The Codex backend binds the OAuth JWT to a specific account via the
// chatgpt-account-id header and fingerprints the client with originator +
// User-Agent; when the token carries the chatgpt_account_id claim we send all
// three, matching the reference clients (pi / litellm). Non-ChatGPT tokens
// have no such claim, so this is a no-op for every other provider.
// Operator-supplied extra_headers (already merged into base) take precedence.
//
// It must run against the final token — not the pre-refresh one — because a
// refresh can rotate in a token whose account claim differs (or was absent
// before).
func finalizeOAuthHeaders(base map[string]string, accessToken string) map[string]string {
	out := make(map[string]string, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
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
	return out
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
