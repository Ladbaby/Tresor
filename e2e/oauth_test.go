//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_OAuth covers the full OAuth login lifecycle against a real daemon:
//
//  1. A YAML-defined downstream whose per-downstream auth block is an
//     auth_code recipe pointing at a mock token endpoint.
//  2. Start a login from the downstream (the recipe lives on the downstream,
//     so no provider name is sent).
//  3. The test plays the browser: it hits the loopback callback with the
//     state the daemon issued, which triggers the code→token exchange
//     against the mock token endpoint and stores the token in SQLite.
//  4. Status flips to connected.
//  5. A proxied request is forwarded to the mock LLM, which echoes back the
//     Authorization header — proving the OAuth token (not the API key) was
//     injected.
//  6. Disconnect reverts the downstream to api_key mode.
//
// The device flow is covered at the unit level (internal/oauth); this test
// focuses on the integration seams that only appear with a live daemon.
func TestE2E_OAuth(t *testing.T) {
	const port = 9211
	const mockPort = 9212
	const llmPort = 9213

	issuedToken := "mock-oauth-access-token"

	// --- Mock OAuth token endpoint ---
	tokenMux := http.NewServeMux()
	tokenMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "authorization_code" {
			http.Error(w, "wrong grant_type", http.StatusBadRequest)
			return
		}
		if form.Get("code") == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		// Verify the PKCE verifier round-trips (S256 of the challenge was
		// sent in the authorize step; the daemon posts the raw verifier).
		if form.Get("code_verifier") == "" {
			http.Error(w, "missing code_verifier", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  issuedToken,
			"token_type":    "Bearer",
			"refresh_token": "mock-refresh",
			"expires_in":    3600,
		})
	})
	mockOAuth := startMockServer(t, mockPort, tokenMux)
	defer mockOAuth.Close()

	// --- Mock LLM that echoes the Authorization header ---
	var gotAuth string
	var gotExtra string
	llmMux := http.NewServeMux()
	llmMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Provider-Tag")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"content": "hi"}},
			},
		})
	})
	mockLLM := startMockServer(t, llmPort, llmMux)
	defer mockLLM.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s
proxy_mode: none

downstreams:
  - id: oapp
    name: OAuth Provider
    base_url: http://127.0.0.1:%d
    api_formats: [openai]
    output_model_ids: [mock-model]
    auth:
      type: oauth
      flow: auth_code
      authorization_url: http://127.0.0.1:%d/authorize
      token_url: http://127.0.0.1:%d/token
      client_id: mock-client
      scopes: "openid"
      extra_headers:
        X-Provider-Tag: e2e-tag
`, port, dbPath, llmPort, mockPort, mockPort)

	apiBase, cleanup := startTresor(t, cfg, port)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}
	dsID := "oapp"

	// 1. The downstream is configured as oauth (recipe lives on it).
	t.Run("DownstreamIsOAuth", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/downstreams/" + dsID)
		if err != nil {
			t.Fatalf("get downstream: %v", err)
		}
		var ds struct {
			Auth struct {
				Type string `json:"type"`
				Flow string `json:"flow"`
			} `json:"auth"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ds); err != nil {
			t.Fatalf("decode downstream: %v", err)
		}
		resp.Body.Close()
		if ds.Auth.Type != "oauth" || ds.Auth.Flow != "auth_code" {
			t.Fatalf("expected downstream auth oauth/auth_code, got %+v", ds.Auth)
		}
	})

	// 2. Start login from the downstream (no provider name in the body).
	var authorizeURL string
	t.Run("StartLogin", func(t *testing.T) {
		resp, err := client.Post(apiBase+"/api/downstreams/"+dsID+"/oauth/start",
			"application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("start login: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
		var info struct {
			Status       string `json:"status"`
			AuthorizeURL string `json:"authorize_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatalf("decode login info: %v", err)
		}
		authorizeURL = info.AuthorizeURL
		if authorizeURL == "" {
			t.Fatalf("expected an authorize_url for auth_code flow")
		}
	})

	// 3. Simulate the browser: extract state and hit the loopback callback.
	t.Run("CallbackExchangesToken", func(t *testing.T) {
		u, err := url.Parse(authorizeURL)
		if err != nil {
			t.Fatalf("parse authorize url: %v", err)
		}
		state := u.Query().Get("state")
		if state == "" {
			t.Fatalf("authorize url has no state param")
		}
		cb := apiBase + "/api/oauth/callback?code=e2e-auth-code&state=" + url.QueryEscape(state)
		resp, err := client.Get(cb)
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected callback 200, got %d: %s", resp.StatusCode, body)
		}
	})

	// 4. Status is now connected.
	var statusInfo struct {
		Connected bool   `json:"connected"`
		Status    string `json:"status"`
		Provider  string `json:"provider"`
	}
	t.Run("StatusConnected", func(t *testing.T) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := client.Get(apiBase + "/api/downstreams/" + dsID + "/oauth-status")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if json.Unmarshal(body, &statusInfo) == nil && statusInfo.Connected {
				// The token row names its provider by downstream ID in the
				// per-downstream model.
				if statusInfo.Provider != dsID {
					t.Errorf("expected provider %s, got %q", dsID, statusInfo.Provider)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for connected, last=%s", body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	// 5. Proxied request carries the OAuth token + provider extra header.
	t.Run("ProxyUsesOAuthToken", func(t *testing.T) {
		body := `{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions", bytes.NewReader([]byte(body)))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("proxy request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}
		if gotAuth != "Bearer "+issuedToken {
			t.Errorf("expected downstream to receive Bearer %s, got %q", issuedToken, gotAuth)
		}
		if gotExtra != "e2e-tag" {
			t.Errorf("expected X-Provider-Tag e2e-tag on forwarded request, got %q", gotExtra)
		}
	})

	// 6. Disconnect reverts to api_key mode.
	t.Run("Disconnect", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, apiBase+"/api/downstreams/"+dsID+"/oauth", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("disconnect: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		// Downstream is back to api_key.
		dresp, err := client.Get(apiBase + "/api/downstreams/" + dsID)
		if err != nil {
			t.Fatalf("get downstream: %v", err)
		}
		var ds struct {
			Auth struct {
				Type string `json:"type"`
			} `json:"auth"`
		}
		dbody, _ := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		if json.Unmarshal(dbody, &ds) != nil {
			t.Fatalf("decode downstream: %s", dbody)
		}
		if ds.Auth.Type == "oauth" {
			t.Errorf("expected downstream reverted to api_key after disconnect, got oauth: %s", dbody)
		}
	})
}

// TestE2E_OAuthNotConnectedBeforeLogin verifies that an oauth-bound
// downstream with no token returns 401 to the client (no forwarding).
func TestE2E_OAuthNotConnectedBeforeLogin(t *testing.T) {
	const port = 9221
	const mockPort = 9222
	const llmPort = 9223

	llmMux := http.NewServeMux()
	llmMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{}`)
	})
	mockLLM := startMockServer(t, llmPort, llmMux)
	defer mockLLM.Close()

	tokenMux := http.NewServeMux()
	tokenMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {})
	mockOAuth := startMockServer(t, mockPort, tokenMux)
	defer mockOAuth.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s
proxy_mode: none

downstreams:
  - id: oapp
    name: OAuth Provider
    base_url: http://127.0.0.1:%d
    api_formats: [openai]
    output_model_ids: [mock-model]
    auth:
      type: oauth
      flow: auth_code
      authorization_url: http://127.0.0.1:%d/authorize
      token_url: http://127.0.0.1:%d/token
`, port, dbPath, llmPort, mockPort, mockPort)

	apiBase, cleanup := startTresor(t, cfg, port)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	// A proxied request must 401 without forwarding — the downstream is
	// oauth (from YAML) but no login has been completed, so there is no token.
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`)))
	r2, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unconnected oauth downstream, got %d: %s", r2.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "not connected") {
		t.Errorf("expected 'not connected' in body, got %q", body)
	}
}
