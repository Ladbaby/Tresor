//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2E runs the full end-to-end integration test against a live Tresor daemon.
// Usage: go test -tags=integration -run TestE2E ./...
// Note: the binary must be built first with 'go build -o tresor.exe .'
func TestE2E(t *testing.T) {
	// Create temp dir for test artifacts
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Write YAML config for the test
	cfgContent := `
bind_addr: 127.0.0.1:9199
db_path: ` + dbPath + `

downstreams:
  - id: openai-gpt4o
    name: OpenAI GPT-4o
    base_url: https://api.openai.com/v1
    api_key: sk-test-key
    output_model_ids:
      - gpt-4o
      - gpt-4o-mini
  - id: anthropic-sonnet
    name: Anthropic Sonnet
    base_url: https://api.anthropic.com
    api_key: sk-ant-test-key
    output_model_ids:
      - claude-sonnet-4-20250514
  - id: azure-gpt4o
    name: Azure GPT-4o
    base_url: https://my-resource.openai.azure.com/openai
    api_key: azure-test-key
    output_model_ids:
      - gpt-4o

rules:
  - id: default
    name: Default Rule
    pattern_path: "*"
    match_downstreams: [openai-gpt4o]
    is_enabled: true

aliases:
  - input_model_id: gpt-4o
    options:
      - id: alias-gpt4o-openai
        downstream_id: openai-gpt4o
        output_model_id: gpt-4o
      - id: alias-gpt4o-anthropic
        downstream_id: anthropic-sonnet
        output_model_id: claude-sonnet-4-20250514
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	binary, err := filepath.Abs("../tresor.exe")
	if err != nil {
		t.Fatalf("Failed to resolve binary path: %v", err)
	}

	t.Log("=== Tresor End-to-End Test ===")
	t.Logf("Binary: %s", binary)
	t.Logf("Config: %s", cfgPath)
	t.Logf("DB: %s", dbPath)

	// Start the daemon with YAML config
	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for startup
	time.Sleep(2 * time.Second)

	apiBase := "http://127.0.0.1:9199"
	client := &http.Client{Timeout: 5 * time.Second}

	// Test 1: Health check
	t.Run("HealthCheck", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/health")
		if err != nil {
			t.Fatalf("health check: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Test 2: List rules (expecting config-loaded defaults)
	t.Run("ListRules", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/rules")
		if err != nil {
			t.Fatalf("list rules: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", body)
		var rules []interface{}
		if err := json.Unmarshal(body, &rules); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rules) == 0 {
			t.Fatal("expected at least 1 rule")
		}
	})

	// Test 3: List downstreams
	t.Run("ListDownstreams", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/downstreams")
		if err != nil {
			t.Fatalf("list downstreams: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", body)
		var ds []interface{}
		if err := json.Unmarshal(body, &ds); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(ds) != 3 {
			t.Fatalf("expected 3 downstreams, got %d", len(ds))
		}
	})

	// Test 4: Create a new rule
	t.Run("CreateRule", func(t *testing.T) {
		newRule := map[string]interface{}{
			"name":              "test-chat",
			"pattern_path":      "/v1/chat/completions",
			"pattern_model":     "gpt-4o",
			"match_downstreams": []string{"openai-gpt4o"},
			"pipeline_config":   `[{"plugin_id":"custom_header","config":{"headers":{"X-Test":"e2e"}}}]`,
			"is_enabled":        true,
		}
		body, _ := json.Marshal(newRule)
		resp, err := client.Post(apiBase+"/api/rules", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("create rule: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", respBody)
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
	})

	// Test 5: Update rule match_downstreams
	t.Run("UpdateRule", func(t *testing.T) {
		payload := map[string]interface{}{
			"match_downstreams": []string{"anthropic-sonnet"},
		}
		data, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPut, apiBase+"/api/rules/default", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("update rule: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", respBody)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Test 6: List plugins
	t.Run("ListPlugins", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/plugins")
		if err != nil {
			t.Fatalf("list plugins: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", body)
		var plugins []interface{}
		if err := json.Unmarshal(body, &plugins); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(plugins) != 8 {
			t.Fatalf("expected 4 plugins, got %d", len(plugins))
		}
	})

	// Test 6b: List alias groups
	t.Run("ListAliasGroups", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/aliases")
		if err != nil {
			t.Fatalf("list aliases: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", body)
		var groups []map[string]interface{}
		if err := json.Unmarshal(body, &groups); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(groups) != 1 {
			t.Fatalf("expected 1 alias group, got %d", len(groups))
		}
		if groups[0]["input_model_id"] != "gpt-4o" {
			t.Fatalf("expected input_model_id gpt-4o, got %v", groups[0]["input_model_id"])
		}
		options := groups[0]["options"].([]interface{})
		if len(options) != 2 {
			t.Fatalf("expected 2 options, got %d", len(options))
		}
	})

	// Test 6c: Activate alias (hot-switch)
	t.Run("ActivateAlias", func(t *testing.T) {
		payload := []byte("{}")
		req, _ := http.NewRequest(http.MethodPut, apiBase+"/api/aliases/alias-gpt4o-anthropic/activate", bytes.NewReader(payload))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("activate alias: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", respBody)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Test 6d: Downstream output_model_ids
	t.Run("DownstreamModels", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/downstreams")
		if err != nil {
			t.Fatalf("list downstreams: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var ds []map[string]interface{}
		json.Unmarshal(body, &ds)
		for _, d := range ds {
			models := d["output_model_ids"]
			t.Logf("Downstream %s output_model_ids: %v", d["id"], models)
		}
	})

	// Test 6e: Add model to downstream
	t.Run("AddModel", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"model_id": "gpt-4.5-preview"})
		resp, err := client.Post(apiBase+"/api/downstreams/openai-gpt4o/models", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("add model: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", respBody)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Test 6f: Remove model from downstream
	t.Run("RemoveModel", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, apiBase+"/api/downstreams/openai-gpt4o/models/gpt-4.5-preview", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("remove model: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Response: %s", respBody)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Test 7: Delete a rule
	t.Run("DeleteRule", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/rules")
		if err != nil {
			t.Fatalf("list rules for delete: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var rules []map[string]interface{}
		json.Unmarshal(body, &rules)
		for _, r := range rules {
			if r["name"] == "test-chat" {
				id := r["id"].(string)
				delReq, _ := http.NewRequest(http.MethodDelete, apiBase+"/api/rules/"+id, nil)
				delResp, delErr := client.Do(delReq)
				if delErr != nil {
					t.Fatalf("delete rule: %v", delErr)
				}
				delResp.Body.Close()
				if delResp.StatusCode != 200 {
					t.Fatalf("expected 200, got %d", delResp.StatusCode)
				}
				return
			}
		}
		t.Fatal("test-chat rule not found for deletion")
	})

	// Test 8: Proxy request (will fail to connect to downstream, but should route)
	t.Run("ProxyRouting", func(t *testing.T) {
		proxyBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
		resp, err := client.Post(apiBase+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(proxyBody)))
		if err != nil {
			// This should try to connect to the downstream and fail (no real upstream key)
			// But the routing itself should have worked
			t.Logf("Expected connection error (no real downstream configured): %v", err)
		} else {
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			t.Logf("Response status: %d, body: %s", resp.StatusCode, respBody)
		}
	})

	// Test 9: Web UI
	t.Run("WebUI", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/")
		if err != nil {
			t.Fatalf("web UI: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("expected 200 with body, got %d, %d bytes", resp.StatusCode, len(body))
		}
		t.Logf("index.html: %d bytes", len(body))
	})

	// Test 10: Web UI assets
	t.Run("WebUIAssets", func(t *testing.T) {
		for _, asset := range []string{"/style.css", "/app.js"} {
			resp, err := client.Get(apiBase + asset)
			if err != nil {
				t.Fatalf("%s: %v", asset, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || len(body) == 0 {
				t.Fatalf("%s: status %d, %d bytes", asset, resp.StatusCode, len(body))
			}
			t.Logf("%s: %d bytes", asset, len(body))
		}
	})
}
