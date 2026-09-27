package oauth

import (
	"context"
	"fmt"
	"log"
	"net/url"
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
		return t.AccessToken, extra, nil
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
			return t2.AccessToken, extra, nil
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
	return nt.AccessToken, extra, nil
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
