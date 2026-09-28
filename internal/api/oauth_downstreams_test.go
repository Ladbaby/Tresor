package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tresor/internal/oauth"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

// validOAuthPatch returns a valid auth_code oauth auth block for PUT/POST.
func validOAuthPatch() map[string]interface{} {
	return map[string]interface{}{
		"type":              "oauth",
		"flow":              "auth_code",
		"authorization_url": "https://auth.first.local/authorize",
		"token_url":         "https://auth.first.local/token",
	}
}

// newOAuthTestRouter builds a test router with a live OAuth manager attached
// (so disconnect/connect operations are observable). Provider recipes no
// longer live in a registry — they are read from each downstream's auth.
func newOAuthTestRouter(t *testing.T) *Router {
	t.Helper()
	r := newTestRouter(t)
	mgr := oauth.NewManager(r.store, proxy.ModeNone)
	r.SetOAuthManager(mgr)
	return r
}

func putDownstream(t *testing.T, handler http.Handler, id string, patch map[string]interface{}) int {
	t.Helper()
	data, _ := json.Marshal(patch)
	req := httptest.NewRequest(http.MethodPut, "/api/downstreams/"+id, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

// TestToggleToOAuth verifies the UI's Authentication toggle succeeds when it
// sends a complete oauth auth block.
func TestToggleToOAuth(t *testing.T) {
	r := newTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "toggle-ds", "https://api.test.com")

	code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": validOAuthPatch(),
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200 toggling to oauth, got %d", code)
	}

	got, err := r.store.GetDownstream(ds.ID)
	if err != nil {
		t.Fatalf("get downstream: %v", err)
	}
	if !got.IsOAuth() {
		t.Errorf("expected downstream to be oauth, got %+v", got.Auth)
	}
}

// TestToggleToOAuth_InvalidRejected ensures an incomplete oauth block is
// rejected with a 400 (the downstream is left in its previous mode).
func TestToggleToOAuth_InvalidRejected(t *testing.T) {
	r := newTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "toggle-ds-2", "https://api.test.com")

	// auth_code without the required URLs must fail validation.
	code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": map[string]interface{}{
			"type": "oauth",
			"flow": "auth_code",
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid oauth block, got %d", code)
	}
	got, _ := r.store.GetDownstream(ds.ID)
	if got.IsOAuth() {
		t.Errorf("downstream must not be left in oauth mode when the switch is rejected")
	}
}

// TestToggleToAPIKeyFromOAuth verifies switching back to api_key clears the
// oauth binding (and would clear the token via the manager when present).
func TestToggleToAPIKeyFromOAuth(t *testing.T) {
	r := newOAuthTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "toggle-ds-3", "https://api.test.com")
	if code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": validOAuthPatch(),
	}); code != http.StatusOK {
		t.Fatalf("expected 200 switching to oauth, got %d", code)
	}

	code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": map[string]interface{}{"type": "api_key", "api_key": "sk-new"},
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200 switching back to api_key, got %d", code)
	}
	got, _ := r.store.GetDownstream(ds.ID)
	if got.IsOAuth() {
		t.Errorf("expected api_key after switch, got %+v", got.Auth)
	}
	if got.EffectiveAPIKey() != "sk-new" {
		t.Errorf("expected api_key sk-new, got %q", got.EffectiveAPIKey())
	}
}

// TestOAuthDisconnect_RevertsToAPIKey verifies DELETE /api/downstreams/{id}/
// oauth reverts the downstream to api_key mode.
func TestOAuthDisconnect_RevertsToAPIKey(t *testing.T) {
	r := newOAuthTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "toggle-ds-4", "https://api.test.com")
	if code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": validOAuthPatch(),
	}); code != http.StatusOK {
		t.Fatalf("expected 200 switching to oauth, got %d", code)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/downstreams/"+ds.ID+"/oauth", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on disconnect, got %d", w.Code)
	}

	got, _ := r.store.GetDownstream(ds.ID)
	if got.IsOAuth() {
		t.Errorf("expected api_key after disconnect, got %+v", got.Auth)
	}
}

// TestOAuthStart_NoManagerRejected ensures start returns a clear 400 when no
// OAuth manager is attached.
func TestOAuthStart_NoManagerRejected(t *testing.T) {
	r := newTestRouter(t) // no OAuth manager attached
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "toggle-ds-5", "https://api.test.com")

	req := httptest.NewRequest(http.MethodPost, "/api/downstreams/"+ds.ID+"/oauth/start", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when no oauth manager, got %d", w.Code)
	}
}

// TestClientSecret_MaskedAndPreserved verifies an oauth client_secret is
// masked on read, and that echoing the mask back in a PUT preserves the real
// stored secret (mirroring the api_key "***" contract).
func TestClientSecret_MaskedAndPreserved(t *testing.T) {
	r := newTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "conf-ds", "https://api.test.com")

	// Create an oauth downstream with a client_secret.
	code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": map[string]interface{}{
			"type":              "oauth",
			"flow":              "auth_code",
			"authorization_url": "https://auth.test/authorize",
			"token_url":         "https://auth.test/token",
			"client_id":         "id",
			"client_secret":     "real-secret",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200 saving oauth auth, got %d", code)
	}

	// Masked on a plain GET.
	getReq := httptest.NewRequest(http.MethodGet, "/api/downstreams/"+ds.ID, nil)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, getReq)
	var got struct {
		Auth struct {
			ClientSecret string `json:"client_secret"`
		} `json:"auth"`
	}
	json.NewDecoder(getW.Body).Decode(&got)
	if got.Auth.ClientSecret != "***" {
		t.Fatalf("expected client_secret masked to ***, got %q", got.Auth.ClientSecret)
	}

	// Patch echoing the mask preserves the real secret.
	code = putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": map[string]interface{}{
			"type":              "oauth",
			"flow":              "auth_code",
			"authorization_url": "https://auth.test/authorize",
			"token_url":         "https://auth.test/token",
			"client_id":         "id",
			"client_secret":     "***",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200 on masked patch, got %d", code)
	}
	stored, _ := r.store.GetDownstream(ds.ID)
	if stored.Auth == nil || stored.Auth.ClientSecret != "real-secret" {
		t.Fatalf("client_secret changed despite *** placeholder, got %+v", stored.Auth)
	}
}

// TestFetchModels_CodexBackend_NoModelsEndpoint verifies the ChatGPT/Codex
// backend — which has no OpenAI-style /models endpoint — returns a clear,
// accurate "no models endpoint" message instead of probing and misreporting
// "authentication failed — check the API key".
func TestFetchModels_CodexBackend_NoModelsEndpoint(t *testing.T) {
	r := newOAuthTestRouter(t)
	handler := r.Handler()

	ds := createDownstreamViaAPI(t, handler, "codex-ds", "https://chatgpt.com/backend-api/codex")
	if code := putDownstream(t, handler, ds.ID, map[string]interface{}{
		"auth": map[string]interface{}{
			"type":     "oauth",
			"flow":     "device",
			"client_id": "app_test",
			"device_auth_url":   "https://auth.test/usercode",
			"device_token_url":  "https://auth.test/token",
			"device_verify_url": "https://auth.test/verify",
			"token_url":         "https://auth.test/oauth/token",
		},
	}); code != http.StatusOK {
		t.Fatalf("expected 200 switching to oauth device, got %d", code)
	}
	got, _ := r.store.GetDownstream(ds.ID)
	if !got.IsOAuth() {
		t.Fatalf("expected downstream to be oauth, got %+v", got.Auth)
	}

	// Seed a token whose extra map carries chatgpt-account-id — the header the
	// OAuth manager surfaces for the real Codex backend — so ResolveValidToken
	// returns it without a refresh.
	if err := r.store.SaveOAuthToken(&store.OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "codex",
		Flow:         "device",
		AccessToken:  "fake-access-token",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Extra:        map[string]interface{}{"chatgpt-account-id": "acct-test"},
	}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	_, err := r.fetchModels(got)
	if err == nil {
		t.Fatal("expected an error for a Codex backend with no models endpoint")
	}
	if strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "check the API key") {
		t.Fatalf("misleading auth error for a Codex backend: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "no models endpoint") {
		t.Fatalf("expected a 'no models endpoint' message, got %q", err.Error())
	}
}
