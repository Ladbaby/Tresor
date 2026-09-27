package oauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tresor/internal/config"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

// newTestStoreAndManager opens a temp store and a manager wired to it.
func newTestStoreAndManager(t *testing.T) (*store.Store, *Manager) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	m := NewManager(s, proxy.ModeNone)
	return s, m
}

func mustDownstream(t *testing.T, s *store.Store) *store.Downstream {
	t.Helper()
	ds := &store.Downstream{Name: "ChatGPT", BaseURL: "https://chatgpt.com/backend-api/codex", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	return ds
}

// authCodeAuth builds an oauth auth_code auth config pointing at tokenURL.
func authCodeAuth(tokenURL string) *config.DownstreamAuthCfg {
	return &config.DownstreamAuthCfg{
		Type:             "oauth",
		Flow:             FlowAuthCode,
		AuthorizationURL: "https://a.test/auth",
		TokenURL:         tokenURL,
	}
}

// bindOAuth persists an oauth auth config on the downstream so the manager
// (which reads auth from the store) can resolve the provider.
func bindOAuth(t *testing.T, s *store.Store, ds *store.Downstream, auth *config.DownstreamAuthCfg) {
	t.Helper()
	if err := s.SetDownstreamAuth(ds.ID, auth); err != nil {
		t.Fatalf("set oauth auth: %v", err)
	}
}

// TestResolveValidToken_ReturnsStoredToken verifies a valid stored token is
// returned without hitting the network.
func TestResolveValidToken_ReturnsStoredToken(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	bindOAuth(t, s, ds, authCodeAuth("https://t.test/token"))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  "tok-1",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	tok, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("token: %q", tok)
	}
	if len(extra) != 0 {
		t.Fatalf("unexpected extra headers: %v", extra)
	}
}

// TestResolveValidToken_NotConnected returns ErrNotConnected when no token
// row exists.
func TestResolveValidToken_NotConnected(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, authCodeAuth("https://t.test/token"))

	if _, _, err := m.ResolveValidToken(ds.ID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

// TestResolveValidToken_RefreshesExpiredToken exercises the refresh path: an
// expired token with a refresh_token triggers a grant_type=refresh_token call
// and the new access token is persisted and returned.
func TestResolveValidToken_RefreshesExpiredToken(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	var gotForm string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm.Encode()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "refreshed-tok",
			"refresh_token": "refreshed-rt",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	bindOAuth(t, s, ds, authCodeAuth(ts.URL))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  "expired",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	tok, _, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "refreshed-tok" {
		t.Fatalf("token: %q", tok)
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") || !strings.Contains(gotForm, "refresh_token=old-rt") {
		t.Fatalf("refresh form: %q", gotForm)
	}
	// New token persisted
	got, _ := s.GetOAuthToken(ds.ID)
	if got.AccessToken != "refreshed-tok" || got.RefreshToken != "refreshed-rt" {
		t.Fatalf("persisted: %+v", got)
	}
	if got.NeedsLogin {
		t.Fatal("needs_login should be false after successful refresh")
	}
}

// TestResolveValidToken_RefreshFailureMarksNeedsLogin verifies a failed
// refresh sets needs_login and subsequent resolves return ErrNotConnected
// without further network calls.
func TestResolveValidToken_RefreshFailureMarksNeedsLogin(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer ts.Close()

	bindOAuth(t, s, ds, authCodeAuth(ts.URL))
	_ = s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  "expired",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})

	if _, _, err := m.ResolveValidToken(ds.ID); err == nil {
		t.Fatal("expected refresh failure error")
	}
	got, _ := s.GetOAuthToken(ds.ID)
	if !got.NeedsLogin {
		t.Fatal("needs_login not set after refresh failure")
	}
	// Second call should short-circuit with ErrNotConnected (no new HTTP call).
	before := calls
	if _, _, err := m.ResolveValidToken(ds.ID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
	if calls != before {
		t.Fatalf("unexpected extra token endpoint calls: %d -> %d", before, calls)
	}
}

// TestResolveValidToken_ConcurrentRefresh ensures concurrent resolves for the
// same downstream produce a single refresh call (per-downstream lock +
// double-check).
func TestResolveValidToken_ConcurrentRefresh(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	var calls int32
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // widen the race window
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "tok",
			"refresh_token": "rt",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	bindOAuth(t, s, ds, authCodeAuth(ts.URL))
	_ = s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  "expired",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.ResolveValidToken(ds.ID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resolve error: %v", err)
	}
	mu.Lock()
	c := calls
	mu.Unlock()
	if c != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", c)
	}
}

// TestHandleCallback_CompletesLogin walks the auth_code flow end-to-end:
// StartLogin returns an authorize URL with a PKCE challenge, the callback
// exchanges the code and persists the token.
func TestHandleCallback_CompletesLogin(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	m.SetCallbackBase("127.0.0.1:11510")

	var gotCodeVerifier string
	var gotRedirectURI string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotCodeVerifier = r.PostForm.Get("code_verifier")
		gotRedirectURI = r.PostForm.Get("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "final-tok",
			"refresh_token": "final-rt",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:             "oauth",
		Flow:             FlowAuthCode,
		AuthorizationURL: "https://a.test/auth",
		TokenURL:         ts.URL,
		ClientID:         "cid",
	})

	info, err := m.StartLogin(ds.ID)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	if !strings.Contains(info.AuthorizeURL, "code_challenge_method=S256") || !strings.Contains(info.AuthorizeURL, "response_type=code") {
		t.Fatalf("authorize url: %q", info.AuthorizeURL)
	}
	if info.RedirectURI != "http://127.0.0.1:11510/api/oauth/callback" {
		t.Fatalf("redirect uri: %q", info.RedirectURI)
	}

	// Downstream switched to oauth mode immediately.
	gotDs, _ := s.GetDownstream(ds.ID)
	if !gotDs.IsOAuth() {
		t.Fatalf("downstream auth: %+v", gotDs)
	}

	// Simulate the provider redirecting back with the code + state.
	state := extractParam(t, info.AuthorizeURL, "state")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/oauth/callback?code=the-code&state="+state, nil)
	m.HandleCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status: %d body=%s", rec.Code, rec.Body.String())
	}
	if gotCodeVerifier == "" {
		t.Fatal("code_verifier not sent to token endpoint")
	}
	if gotRedirectURI != "http://127.0.0.1:11510/api/oauth/callback" {
		t.Fatalf("token exchange redirect_uri: %q", gotRedirectURI)
	}
	tok, err := s.GetOAuthToken(ds.ID)
	if err != nil || tok == nil {
		t.Fatalf("token not stored: %v", err)
	}
	if tok.AccessToken != "final-tok" {
		t.Fatalf("stored token: %q", tok.AccessToken)
	}

	// Bad state must be rejected.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/oauth/callback?code=x&state=wrong", nil)
	m.HandleCallback(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("bad state should 400, got %d", rec2.Code)
	}
}

// TestDisconnect reverts a downstream to api_key mode and drops its token.
func TestDisconnect(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, authCodeAuth("https://t.test/token"))
	_ = s.SaveOAuthToken(&store.OAuthToken{DownstreamID: ds.ID, Provider: "p", Flow: FlowAuthCode, AccessToken: "a"})

	if err := m.Disconnect(ds.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDownstream(ds.ID)
	if got.IsOAuth() {
		t.Fatalf("auth after disconnect: %+v", got)
	}
	tok, _ := s.GetOAuthToken(ds.ID)
	if tok != nil {
		t.Fatalf("token should be gone: %+v", tok)
	}
}

// TestNormalize_Validation covers required-field and discovery handling.
func TestNormalize_Validation(t *testing.T) {
	// Missing flow
	if _, err := Normalize(config.DownstreamAuthCfg{Type: "oauth"}, http.DefaultClient); err == nil {
		t.Fatal("expected error for missing flow")
	}
	// auth_code missing URLs
	if _, err := Normalize(config.DownstreamAuthCfg{Type: "oauth", Flow: FlowAuthCode}, http.DefaultClient); err == nil {
		t.Fatal("expected error for missing auth_code URLs")
	}
	// non-https URL
	if _, err := Normalize(config.DownstreamAuthCfg{Type: "oauth", Flow: FlowAuthCode, AuthorizationURL: "http://a", TokenURL: "https://b"}, http.DefaultClient); err == nil {
		t.Fatal("expected error for non-https authorization_url")
	}
	// device missing fields
	if _, err := Normalize(config.DownstreamAuthCfg{Type: "oauth", Flow: FlowDevice}, http.DefaultClient); err == nil {
		t.Fatal("expected error for missing device URLs")
	}
	// discovery fills endpoints
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": "https://disc.test/auth",
			"token_endpoint":         "https://disc.test/token",
		})
	}))
	defer ts.Close()
	p, err := Normalize(config.DownstreamAuthCfg{Type: "oauth", Flow: FlowAuthCode, DiscoveryURL: ts.URL}, http.DefaultClient)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if p.AuthorizationURL != "https://disc.test/auth" || p.TokenURL != "https://disc.test/token" {
		t.Fatalf("discovered: %+v", p)
	}
}

// TestJWTExp extracts exp from a real JWT payload.
func TestJWTExp(t *testing.T) {
	// header.payload.sig — payload is base64url of {"exp": 1234567890}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1234567890}`))
	token := "aaaa." + payload + ".cccc"
	if got := jwtExp(token); got != 1234567890 {
		t.Fatalf("exp: %d", got)
	}
	if got := jwtExp("not-a-jwt"); got != 0 {
		t.Fatalf("non-jwt should be 0, got %d", got)
	}
}

// TestDeviceFlow_PollToCompletion exercises the device flow: StartLogin
// returns a user code, the poller detects authorization and stores the token.
func TestDeviceFlow_PollToCompletion(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	mu := sync.Mutex{}
	pollCount := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pollCount++
		isPoll := pollCount >= 2
		mu.Unlock()
		if !isPoll {
			// First call is the device-auth request
			json.NewEncoder(w).Encode(map[string]interface{}{
				"device_auth_id": "da-1",
				"user_code":      "ABCD-EFGH",
				"interval":       1,
			})
			return
		}
		// Poll: first poll = pending, second = authorized
		mu.Lock()
		p := pollCount
		mu.Unlock()
		if p == 2 {
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "device-tok", "expires_in": 3600})
	}))
	defer server.Close()

	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:            "oauth",
		Flow:            FlowDevice,
		DeviceAuthURL:   server.URL + "/auth",
		DeviceTokenURL:  server.URL + "/poll",
		DeviceVerifyURL: "https://auth.openai.com/codex/device",
	})

	info, err := m.StartLogin(ds.ID)
	if err != nil {
		t.Fatalf("start device login: %v", err)
	}
	if info.UserCode != "ABCD-EFGH" || info.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("login info: %+v", info)
	}

	// Wait for the poller to complete the login.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := m.Status(ds.ID)
		if st.Connected {
			break
		}
		if st.Status == "failed" {
			t.Fatalf("login failed: %s", st.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for device login")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tok, err := s.GetOAuthToken(ds.ID)
	if err != nil || tok == nil || tok.AccessToken != "device-tok" {
		t.Fatalf("device token: %+v err=%v", tok, err)
	}
}

// TestDeviceFlow_StringIntervalRegression locks in the fix for a provider
// (ChatGPT) that returns device-auth "interval" as a quoted string rather
// than a JSON number, which a plain int field rejected with
// "cannot unmarshal string into ... type int".
func TestDeviceFlow_StringIntervalRegression(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	mu := sync.Mutex{}
	pollCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pollCount++
		isPoll := pollCount >= 2
		mu.Unlock()
		if !isPoll {
			// ChatGPT returns interval/expires_in as quoted strings.
			json.NewEncoder(w).Encode(map[string]interface{}{
				"device_auth_id": "da-1",
				"user_code":      "ABCD-EFGH",
				"interval":       "1",
				"expires_in":     "600",
			})
			return
		}
		mu.Lock()
		p := pollCount
		mu.Unlock()
		if p == 2 {
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		// expires_in as a string too.
		json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "device-tok", "expires_in": "3600"})
	}))
	defer server.Close()

	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:            "oauth",
		Flow:            FlowDevice,
		DeviceAuthURL:   server.URL + "/auth",
		DeviceTokenURL:  server.URL + "/poll",
		DeviceVerifyURL: "https://auth.openai.com/codex/device",
	})

	if _, err := m.StartLogin(ds.ID); err != nil {
		t.Fatalf("start device login (string interval): %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := m.Status(ds.ID)
		if st.Connected {
			break
		}
		if st.Status == "failed" {
			t.Fatalf("login failed: %s", st.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for device login")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tok, err := s.GetOAuthToken(ds.ID)
	if err != nil || tok == nil || tok.AccessToken != "device-tok" {
		t.Fatalf("device token: %+v err=%v", tok, err)
	}
}

// extractParam pulls a query parameter out of a URL string.
func extractParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	if i := strings.Index(rawURL, "?"); i >= 0 {
		rawURL = rawURL[i+1:]
	}
	for _, kv := range strings.Split(rawURL, "&") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 && parts[0] == key {
			return parts[1]
		}
	}
	t.Fatalf("param %q not found in %q", key, rawURL)
	return ""
}

func base64urlEncode(_ *testing.T, b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// keep fmt import used
var _ = fmt.Sprintf

// makeChatGPTJWT builds a (unsigned) JWT whose payload carries the
// namespaced chatgpt_account_id claim the Codex backend expects, so tests
// can exercise the account-id / originator header injection without a real
// token.
func makeChatGPTJWT(t *testing.T, accountID string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	var payload map[string]interface{} = map[string]interface{}{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": accountID,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return header + "." + body + "."
}

// TestResolveValidToken_ChatGPTHeadersAfterRefresh verifies the fix for a
// stale-headers bug: the ChatGPT identity headers must be derived from the
// FINAL (post-refresh) access token, not the pre-refresh one. Here the stored
// token has no chatgpt claim and is expired; the refresh response is a ChatGPT
// token carrying an account claim, so the resolved headers must reflect it.
func TestResolveValidToken_ChatGPTHeadersAfterRefresh(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	// The refreshed token carries the ChatGPT account claim.
	newTok := makeChatGPTJWT(t, "acct-new", time.Now().Add(time.Hour))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  newTok,
			"refresh_token": "refreshed-rt",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	bindOAuth(t, s, ds, authCodeAuth(ts.URL))
	// Stored token: expired and plain (no chatgpt claim).
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  "expired-plain",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	tok, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != newTok {
		t.Fatalf("token not the refreshed one")
	}
	// Headers must come from the refreshed token's claim, not the stale one.
	if got := extra["chatgpt-account-id"]; got != "acct-new" {
		t.Fatalf("chatgpt-account-id = %q, want acct-new (derived from refreshed token)", got)
	}
	if got := extra["originator"]; got != "pi" {
		t.Fatalf("originator = %q, want pi", got)
	}
}

// TestResolveValidToken_GrokHeaders verifies that an xAI Grok subscription
// (scopes include "grok-cli:access") gets the grok-shell identity headers
// injected, matching the official Grok CLI.
func TestResolveValidToken_GrokHeaders(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:             "oauth",
		Flow:             FlowAuthCode,
		AuthorizationURL: "https://a.test/auth",
		TokenURL:         "https://t.test/token",
		Scopes:           "openid profile email offline_access grok-cli:access api:access",
	})

	// A plain (non-ChatGPT) token — Grok is detected by scopes, not a claim.
	tok := makeChatGPTJWT(t, "", time.Now().Add(time.Hour))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  tok,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := extra["x-grok-client-identifier"]; got != "grok-shell" {
		t.Fatalf("x-grok-client-identifier = %q, want grok-shell", got)
	}
	if got := extra["User-Agent"]; !strings.HasPrefix(got, "grok-shell/") {
		t.Fatalf("User-Agent = %q, want prefix 'grok-shell/'", got)
	}
	// Grok tokens have no chatgpt claim, so no chatgpt-account-id header.
	if _, ok := extra["chatgpt-account-id"]; ok {
		t.Fatal("chatgpt-account-id should not be set for a Grok token")
	}
}

// TestResolveValidToken_GrokHeadersOperatorOverride verifies operator
// extra_headers win over the injected grok-shell identity.
func TestResolveValidToken_GrokHeadersOperatorOverride(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:             "oauth",
		Flow:             FlowAuthCode,
		AuthorizationURL: "https://a.test/auth",
		TokenURL:         "https://t.test/token",
		Scopes:           "grok-cli:access",
		ExtraHeaders:     map[string]string{"x-grok-client-identifier": "mytool"},
	})

	tok := makeChatGPTJWT(t, "", time.Now().Add(time.Hour))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  tok,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := extra["x-grok-client-identifier"]; got != "mytool" {
		t.Fatalf("x-grok-client-identifier = %q, want operator override mytool", got)
	}
}

// TestResolveValidToken_ChatGPTHeaders verifies that when the access token is
// a ChatGPT/Codex token, the chatgpt-account-id, originator and User-Agent
// headers are injected (matching the pi reference client).
func TestResolveValidToken_ChatGPTHeaders(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, authCodeAuth("https://t.test/token"))

	tok := makeChatGPTJWT(t, "acct-123", time.Now().Add(time.Hour))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  tok,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := extra["chatgpt-account-id"]; got != "acct-123" {
		t.Fatalf("chatgpt-account-id = %q, want acct-123", got)
	}
	if got := extra["originator"]; got != "pi" {
		t.Fatalf("originator = %q, want pi", got)
	}
	if got := extra["User-Agent"]; !strings.HasPrefix(got, "pi (") {
		t.Fatalf("User-Agent = %q, want prefix 'pi ('", got)
	}
}

// TestResolveValidToken_ChatGPTHeadersOperatorOverride verifies operator
// extra_headers take precedence over the injected identity.
func TestResolveValidToken_ChatGPTHeadersOperatorOverride(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:             "oauth",
		Flow:             FlowAuthCode,
		AuthorizationURL: "https://a.test/auth",
		TokenURL:         "https://t.test/token",
		ExtraHeaders:     map[string]string{"originator": "mytool"},
	})

	tok := makeChatGPTJWT(t, "acct-999", time.Now().Add(time.Hour))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  tok,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := extra["originator"]; got != "mytool" {
		t.Fatalf("originator = %q, want operator override mytool", got)
	}
	// account id is still injected (no operator value for it)
	if got := extra["chatgpt-account-id"]; got != "acct-999" {
		t.Fatalf("chatgpt-account-id = %q, want acct-999", got)
	}
}

// TestResolveValidToken_NonChatGPTNoHeaders verifies a non-ChatGPT token
// (no chatgpt_account_id claim) does not get the Codex headers.
func TestResolveValidToken_NonChatGPTNoHeaders(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)
	bindOAuth(t, s, ds, authCodeAuth("https://t.test/token"))

	// A plain JWT with only an exp claim — no chatgpt claim.
	tok := makeChatGPTJWT(t, "", time.Now().Add(time.Hour))
	if err := s.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         FlowAuthCode,
		AccessToken:  tok,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, extra, err := m.ResolveValidToken(ds.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(extra) != 0 {
		t.Fatalf("unexpected extra headers for non-chatgpt token: %v", extra)
	}
}
