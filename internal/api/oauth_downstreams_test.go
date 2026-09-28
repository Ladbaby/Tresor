package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/oauth"
	"tresor/internal/proxy"
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

// TestFetchCodexModels verifies the ChatGPT/Codex models manifest fetch: it
// GETs {base}/models?client_version=... with the Codex CLI identity headers
// (Originator, Version, chatgpt-account-id) and parses the {models:[{slug}]}
// envelope.
func TestFetchCodexModels(t *testing.T) {
	// Pin the version lookup so the test is offline and deterministic.
	origLookup := codexVersionLookup
	t.Cleanup(func() { codexVersionLookup = origLookup })
	codexVersionLookup = func(ctx context.Context) string { return "0.999.0" }

	var gotOriginator, gotVersion, gotAccount, gotAuth, gotClientVersion, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginator = r.Header.Get("Originator")
		gotVersion = r.Header.Get("Version")
		gotAccount = r.Header.Get("chatgpt-account-id")
		gotAuth = r.Header.Get("Authorization")
		gotClientVersion = r.URL.Query().Get("client_version")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6-luna","id":"x1"},{"slug":"gpt-6-astra","id":"x2"},{"slug":"gpt-6-luna","id":"dup"}]}`)
	}))
	defer server.Close()

	models, err := fetchCodexModels(server.URL, "tok-123", "acct-9")
	if err != nil {
		t.Fatalf("fetchCodexModels returned error: %v", err)
	}
	// Duplicate slug collapsed.
	want := []string{"gpt-6-luna", "gpt-6-astra"}
	if len(models) != len(want) || models[0] != want[0] || models[1] != want[1] {
		t.Fatalf("unexpected models: %v", models)
	}
	if gotPath != "/models" {
		t.Errorf("expected /models path, got %q", gotPath)
	}
	if gotOriginator != codexModelsOriginator {
		t.Errorf("Originator = %q, want %q", gotOriginator, codexModelsOriginator)
	}
	// The resolved (latest) version drives both the query and the Version header.
	if gotVersion != "0.999.0" {
		t.Errorf("Version = %q, want %q", gotVersion, "0.999.0")
	}
	if gotClientVersion != "0.999.0" {
		t.Errorf("client_version query = %q, want %q", gotClientVersion, "0.999.0")
	}
	if gotAccount != "acct-9" {
		t.Errorf("chatgpt-account-id = %q, want acct-9", gotAccount)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
}

// TestFetchCodexModels_VersionFallback verifies that when the GitHub release
// lookup returns nothing (offline), the pinned fallback version is used.
func TestFetchCodexModels_VersionFallback(t *testing.T) {
	origLookup := codexVersionLookup
	t.Cleanup(func() { codexVersionLookup = origLookup })
	codexVersionLookup = func(ctx context.Context) string { return "" }

	var gotVersion, gotClientVersion, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("Version")
		gotClientVersion = r.URL.Query().Get("client_version")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6-luna"}]}`)
	}))
	defer server.Close()

	if _, err := fetchCodexModels(server.URL, "tok", "acct"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotVersion != codexModelsVersion {
		t.Errorf("Version = %q, want fallback %q", gotVersion, codexModelsVersion)
	}
	if gotClientVersion != codexModelsVersion {
		t.Errorf("client_version = %q, want fallback %q", gotClientVersion, codexModelsVersion)
	}
	if want := codexUserAgent(codexModelsVersion); gotUA != want {
		t.Errorf("User-Agent = %q, want %q", gotUA, want)
	}
}

// TestLatestCodexVersion_ParsesName verifies the release "name" (clean semver,
// no "rust-v" prefix) is returned even though the tag carries the prefix.
func TestLatestCodexVersion_ParsesName(t *testing.T) {
	origURL := codexLatestReleaseURL
	t.Cleanup(func() { codexLatestReleaseURL = origURL })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"0.157.1","tag_name":"rust-v0.157.1"}`)
	}))
	defer gh.Close()
	codexLatestReleaseURL = gh.URL

	if got := latestCodexVersion(context.Background()); got != "0.157.1" {
		t.Fatalf("latestCodexVersion = %q, want 0.157.1", got)
	}
}

// TestLatestCodexVersion_FallsBackToTag covers a payload missing "name".
func TestLatestCodexVersion_FallsBackToTag(t *testing.T) {
	origURL := codexLatestReleaseURL
	t.Cleanup(func() { codexLatestReleaseURL = origURL })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"rust-v0.200.5"}`)
	}))
	defer gh.Close()
	codexLatestReleaseURL = gh.URL

	if got := latestCodexVersion(context.Background()); got != "0.200.5" {
		t.Fatalf("latestCodexVersion = %q, want 0.200.5 (from tag)", got)
	}
}

// TestLatestCodexVersion_ReturnsEmptyOnFailure ensures any failure (non-200,
// unparseable, non-numeric version) yields "" so the caller uses the fallback.
func TestLatestCodexVersion_ReturnsEmptyOnFailure(t *testing.T) {
	origURL := codexLatestReleaseURL
	t.Cleanup(func() { codexLatestReleaseURL = origURL })

	cases := map[string]struct {
		status int
		body   string
	}{
		"non-200":        {status: 404, body: `{"message":"not found"}`},
		"unparseable":    {status: 200, body: `not json`},
		"non-numeric":    {status: 200, body: `{"name":"latest"}`},
		"empty-name-tag": {status: 200, body: `{"name":"","tag_name":""}`},
	}
	for name, c := range cases {
		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		codexLatestReleaseURL = gh.URL
		if got := latestCodexVersion(context.Background()); got != "" {
			t.Errorf("%s: latestCodexVersion = %q, want empty", name, got)
		}
		gh.Close()
	}
}

// TestFetchCodexModels_AuthFailure verifies a 401 from the manifest endpoint
// yields a clear "reconnect" message rather than a misleading auth-error.
func TestFetchCodexModels_AuthFailure(t *testing.T) {
	origLookup := codexVersionLookup
	t.Cleanup(func() { codexVersionLookup = origLookup })
	codexVersionLookup = func(ctx context.Context) string { return "" }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := fetchCodexModels(server.URL, "tok", "acct")
	if err == nil {
		t.Fatal("expected an error for a 401 manifest response")
	}
	if !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("expected a token-expired message, got %q", err.Error())
	}
}
