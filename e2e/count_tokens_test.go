//go:build integration

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2E_CountTokens_RetryOnEmpty verifies that POST /v1/messages/count_tokens
// is NOT classified as empty when retry_on_empty is enabled.
//
// Background: IsAnthropicEmpty parses only the `content` field of an
// Anthropic Messages response. A count_tokens reply carries only
// {"input_tokens":<int>}, so the parser incorrectly reports the body as
// empty. retry_on_empty would then retry three times and return 502.
//
// This regression test wires a stub Anthropic-format downstream that
// returns the canonical count_tokens payload, POSTs to the gateway
// with retry_on_empty=true, and asserts:
//  1. The downstream is hit exactly once (no retry loop).
//  2. The client receives the body verbatim.
//  3. The response is 200, not 502.
func TestE2E_CountTokens_RetryOnEmpty(t *testing.T) {
	var calls int32
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only accept the count_tokens path; reject anything else so the
		// test fails loudly if the gateway forwards to the wrong URL.
		if r.URL.Path != "/v1/messages/count_tokens" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"input_tokens":7802}`))
	}))
	defer downstream.Close()

	port := 9198
	dbPath := filepath.Join(t.TempDir(), "count-tokens.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s
retry_on_empty: true

downstreams:
  - id: stub-anthropic
    name: Stub Anthropic
    base_url: %s
    api_key: sk-stub
    api_formats:
      - anthropic
    output_model_ids:
      - claude-sonnet-4-20250514
`,
		port, dbPath, downstream.URL)

	apiBase, cleanup := startTresor(t, cfg, port)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}
	reqBody := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, apiBase+"/v1/messages/count_tokens", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The gateway may validate proxy_api_keys; a dummy Bearer is fine
	// here because the test config does not enable proxy_api_keys.
	req.Header.Set("Authorization", "Bearer sk-dummy")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post count_tokens: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%q)", resp.StatusCode, string(body))
	}
	if got := string(body); got != `{"input_tokens":7802}` {
		t.Errorf("expected body to be passed through verbatim, got %q", got)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected downstream called once (no retry), got %d", got)
	}
	if ctype := resp.Header.Get("Content-Type"); ctype == "" {
		t.Error("expected non-empty Content-Type on response")
	}
}
