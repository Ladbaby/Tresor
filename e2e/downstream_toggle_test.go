//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

const togglePort = 9201

// TestDownstreamToggle verifies the provider ON/OFF toggle behaves "as if
// deleted": an OFF downstream and its models disappear from /v1/models and
// requests for them 404; re-enabling restores them. The row itself is never
// removed (it stays in /api/downstreams so the UI can re-enable it).
func TestDownstreamToggle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: prov-a
    name: Provider A
    base_url: https://a.example.invalid
    api_key: sk-a
    output_model_ids:
      - model-a
  - id: prov-b
    name: Provider B
    base_url: https://b.example.invalid
    api_key: sk-b
    output_model_ids:
      - model-b

aliases:
  - input_model_id: alias-b
    options:
      - id: alias-b-b
        downstream_id: prov-b
        output_model_id: model-b
`,
		togglePort, dbPath)
	apiBase, cleanup := startTresor(t, cfg, togglePort)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	modelsPresent := func(id string) bool {
		resp, err := client.Get(apiBase + "/v1/models")
		if err != nil {
			t.Fatalf("get /v1/models: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal models: %v (body=%s)", err, body)
		}
		for _, m := range out.Data {
			if m.ID == id {
				return true
			}
		}
		return false
	}

	setEnabled := func(id string, enabled bool) {
		body, _ := json.Marshal(map[string]interface{}{"is_enabled": enabled})
		req, _ := http.NewRequest(http.MethodPut, apiBase+"/api/downstreams/"+id, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("put is_enabled: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 disabling %s, got %d body=%s", id, resp.StatusCode, b)
		}
	}

	completionStatus := func(model string) int {
		payload, _ := json.Marshal(map[string]interface{}{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		resp, err := client.Post(apiBase+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("post chat: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	t.Run("InitiallyAllEnabled", func(t *testing.T) {
		if !modelsPresent("model-a") || !modelsPresent("model-b") || !modelsPresent("alias-b") {
			t.Fatalf("expected model-a, model-b and alias-b present in /v1/models initially")
		}
		// prov-b is a mock .invalid endpoint; a live request would 502, not 404.
		if got := completionStatus("alias-b"); got == http.StatusNotFound {
			t.Fatalf("alias-b should resolve while prov-b is enabled, got 404")
		}
	})

	t.Run("DisableHidesAsIfDeleted", func(t *testing.T) {
		setEnabled("prov-b", false)
		if modelsPresent("model-b") {
			t.Fatal("model-b should be hidden from /v1/models after disabling prov-b")
		}
		if modelsPresent("alias-b") {
			t.Fatal("alias-b should be hidden from /v1/models after disabling prov-b")
		}
		if !modelsPresent("model-a") {
			t.Fatal("model-a should remain visible (prov-a still enabled)")
		}
		// Direct model and its alias must now 404 "unknown model".
		if got := completionStatus("model-b"); got != http.StatusNotFound {
			t.Fatalf("model-b should 404 while prov-b disabled, got %d", got)
		}
		if got := completionStatus("alias-b"); got != http.StatusNotFound {
			t.Fatalf("alias-b should 404 while prov-b disabled, got %d", got)
		}
		// The row must still exist in the admin list so the UI can re-enable it.
		resp, err := client.Get(apiBase + "/api/downstreams")
		if err != nil {
			t.Fatalf("list downstreams: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var list []map[string]interface{}
		if err := json.Unmarshal(b, &list); err != nil {
			t.Fatalf("unmarshal downstreams: %v", err)
		}
		found := false
		for _, d := range list {
			if d["id"] == "prov-b" {
				found = true
				if v, _ := d["is_enabled"].(bool); v {
					t.Fatalf("prov-b should report is_enabled=false, got %v", d["is_enabled"])
				}
			}
		}
		if !found {
			t.Fatal("prov-b row must still be listed even while disabled")
		}
	})

	t.Run("ReEnableRestores", func(t *testing.T) {
		setEnabled("prov-b", true)
		if !modelsPresent("model-b") || !modelsPresent("alias-b") {
			t.Fatal("model-b and alias-b should be visible again after re-enabling prov-b")
		}
		if got := completionStatus("model-b"); got == http.StatusNotFound {
			t.Fatal("model-b should resolve again after re-enabling prov-b")
		}
	})
}
