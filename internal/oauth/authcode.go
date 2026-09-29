package oauth

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tresor/internal/store"
)

// randRead fills buf with cryptographically secure random bytes.
func randRead(buf []byte) (int, error) {
	return rand.Read(buf)
}

// HandleCallback serves the loopback OAuth redirect for the auth_code flow.
// The request query must contain code + state; state is verified against
// the pending login (constant-time compare).
func (m *Manager) HandleCallback(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	state := q.Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	m.pendingMu.Lock()
	pl := m.pending[state]
	m.pendingMu.Unlock()

	if pl == nil || !constantTimeEq(state, pl.State) {
		http.Error(w, "invalid or expired state parameter", http.StatusBadRequest)
		return
	}
	if errParam := q.Get("error"); errParam != "" {
		pl.Status = "failed"
		pl.Error = errParam
		m.completePending(pl)
		http.Error(w, "provider returned an error: "+errParam, http.StatusUnauthorized)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	p, err := m.buildProvider(pl.DownstreamID, pl.auth)
	if err != nil {
		m.failPending(pl, err.Error())
		http.Error(w, "invalid oauth configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Exchange the code for tokens.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", m.redirectURIFor(p))
	if p.ClientID != "" {
		form.Set("client_id", p.ClientID)
	}
	form.Set("code_verifier", pl.CodeVerifier)

	tr, err := m.postToken(p, form)
	if err != nil {
		m.failPending(pl, err.Error())
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	tok := m.buildTokenRow(pl.DownstreamID, p, tr)
	if err := m.store.SaveOAuthToken(tok); err != nil {
		m.failPending(pl, err.Error())
		http.Error(w, "failed to save token", http.StatusInternalServerError)
		return
	}
	pl.Status = "success"
	m.completePending(pl)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `<html><body style="font-family:system-ui;text-align:center;padding:4rem">
<h2>Connected ✓</h2><p>You can close this tab and return to the Tresor dashboard.</p>
</body></html>`)
}

// completePending keeps a finished pending around briefly so Status() can
// report success, then removes it.
func (m *Manager) completePending(pl *pendingLogin) {
	m.pendingMu.Lock()
	if pl.State != "" {
		delete(m.pending, pl.State)
	}
	m.pendingByKey[pl.DownstreamID] = pl // keep for status polling
	m.pendingMu.Unlock()

	ttl := 30 * time.Second
	if pl.Status == "pending" {
		ttl = 10 * time.Minute
	}
	go func() {
		time.Sleep(ttl)
		m.pendingMu.Lock()
		if cur := m.pendingByKey[pl.DownstreamID]; cur == pl {
			delete(m.pendingByKey, pl.DownstreamID)
		}
		m.pendingMu.Unlock()
	}()
}

func (m *Manager) failPending(pl *pendingLogin, reason string) {
	pl.Status = "failed"
	pl.Error = reason
	m.completePending(pl)
}

// postToken POSTs form-encoded data to the provider's token endpoint and
// parses the response. Confidential clients authenticate with HTTP Basic.
func (m *Manager) postToken(p *Provider, form url.Values) (*tokenResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if user, pass := p.tokenAuth(); user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = parseTokenResponse(body, resp.Header.Get("Content-Type"))
		// Include the response body so the caller can classify the failure:
		// a 400 body containing "invalid_grant" is a definitive auth rejection,
		// while a network error / 5xx is transient and retryable.
		return nil, fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return parseTokenResponse(body, resp.Header.Get("Content-Type"))
}

// buildTokenRow converts a parsed token response into a store row,
// preserving the old refresh token when the response omits one.
func (m *Manager) buildTokenRow(downstreamID string, p *Provider, tr *tokenResponse) *store.OAuthToken {
	expiresAt := int64(0)
	if tr.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	} else if exp := jwtExp(tr.AccessToken); exp > 0 {
		expiresAt = exp
	}
	extra := map[string]interface{}{}
	if tr.IDToken != "" {
		extra["id_token"] = tr.IDToken
	}
	return &store.OAuthToken{
		DownstreamID: downstreamID,
		Provider:     p.Name,
		Flow:         p.Flow,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}
}
