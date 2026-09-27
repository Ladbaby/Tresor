package oauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"tresor/internal/config"
	"tresor/internal/store"
)

// TestRefresh_ConfidentialClientBasicAuth verifies that a provider with a
// client_secret sends HTTP Basic auth on the token endpoint (RFC 6749 §2.3.1).
func TestRefresh_ConfidentialClientBasicAuth(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	var gotUser, gotPass string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			t.Error("expected Basic auth on token request")
		}
		gotUser, gotPass = user, pass
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-tok",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	auth := authCodeAuth(ts.URL)
	auth.ClientID = "conf-id"
	auth.ClientSecret = "conf-secret"
	bindOAuth(t, s, ds, auth)
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

	if _, _, err := m.ResolveValidToken(ds.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotUser != "conf-id" || gotPass != "conf-secret" {
		t.Fatalf("basic auth: got user=%q pass=%q", gotUser, gotPass)
	}
}

// TestAuthCodeAuthorizeURL_ExtraAuthParams verifies extra_authorize_params
// are appended to the authorize URL returned by StartLogin.
func TestAuthCodeAuthorizeURL_ExtraAuthParams(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	auth := authCodeAuth("https://t.test/token")
	auth.ClientID = "some-client"
	auth.ExtraAuthParams = map[string]string{
		"codex_cli_simplified_flow": "true",
		"plan":                      "generic",
	}
	bindOAuth(t, s, ds, auth)

	info, err := m.StartLogin(ds.ID)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	if info.AuthorizeURL == "" {
		t.Fatal("expected an authorize URL for auth_code flow")
	}
	if got := extractParam(t, info.AuthorizeURL, "codex_cli_simplified_flow"); got != "true" {
		t.Fatalf("codex_cli_simplified_flow: got %q", got)
	}
	if got := extractParam(t, info.AuthorizeURL, "plan"); got != "generic" {
		t.Fatalf("plan: got %q", got)
	}
	// Standard params still present.
	extractParam(t, info.AuthorizeURL, "client_id")
	extractParam(t, info.AuthorizeURL, "code_challenge")
	extractParam(t, info.AuthorizeURL, "state")
}

// TestDeviceFlow_SlowDownIncreasesInterval verifies RFC 8628: after a
// slow_down response the poller waits longer before the next poll.
func TestDeviceFlow_SlowDownIncreasesInterval(t *testing.T) {
	s, m := newTestStoreAndManager(t)
	ds := mustDownstream(t, s)

	var mu sync.Mutex
	pollTimes := []time.Time{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			DeviceCode string `json:"device_code"`
		}
		_ = json.Unmarshal(body, &req)
		// First call has no device_code yet — it is the device-auth request.
		if req.DeviceCode == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"user_code":   "ABCD-EFGH",
				"device_code": "dc-1",
				"interval":    1,
			})
			return
		}
		mu.Lock()
		pollTimes = append(pollTimes, time.Now())
		idx := len(pollTimes)
		mu.Unlock()
		switch idx {
		case 1:
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		case 2, 3:
			json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "device-tok", "expires_in": 3600})
		}
	}))
	defer server.Close()

	bindOAuth(t, s, ds, &config.DownstreamAuthCfg{
		Type:            "oauth",
		Flow:            FlowDevice,
		DeviceAuthURL:   server.URL,
		DeviceTokenURL:  server.URL,
		DeviceVerifyURL: "https://example.com/activate",
	})

	if _, err := m.StartLogin(ds.ID); err != nil {
		t.Fatalf("start device login: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
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

	mu.Lock()
	times := append([]time.Time{}, pollTimes...)
	mu.Unlock()
	if len(times) < 4 {
		t.Fatalf("expected at least 4 polls, got %d", len(times))
	}
	// Gaps: 1→2 is the base interval (~1s); 2→3 and 3→4 must grow because
	// the server returned slow_down each time (doubled, capped at 60s).
	gap1 := times[1].Sub(times[0])
	gap2 := times[2].Sub(times[1])
	gap3 := times[3].Sub(times[2])
	if gap2 < gap1 {
		t.Fatalf("gap after first slow_down (%v) not longer than base gap (%v)", gap2, gap1)
	}
	if gap3 <= gap2 {
		t.Fatalf("gap after second slow_down (%v) not longer than previous (%v)", gap3, gap2)
	}
}
