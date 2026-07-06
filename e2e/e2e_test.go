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

const e2ePort = 9199

func TestE2E(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: openai-gpt4o
    name: OpenAI GPT-4o
    base_url: https://api.openai.com
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
  - id: minimax-test
    name: MiniMax Test
    base_url: https://example.invalid
    api_key: sk-noop
    output_model_ids:
      - MiniMax-M2.5

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
	apiBase, cleanup := startTresor(t, cfg, e2ePort)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("HealthCheck", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/health")
		if err != nil {
			t.Fatalf("health check: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, body)
		}
	})

	t.Run("ListRules", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/rules")
		if err != nil {
			t.Fatalf("list rules: %v", err)
		}
		defer resp.Body.Close()
		var rules []interface{}
		if err := json.NewDecoder(resp.Body).Decode(&rules); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rules) == 0 {
			t.Fatal("expected at least 1 rule")
		}
	})

	t.Run("ListDownstreams", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/downstreams")
		if err != nil {
			t.Fatalf("list downstreams: %v", err)
		}
		defer resp.Body.Close()
		var ds []interface{}
		if err := json.NewDecoder(resp.Body).Decode(&ds); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(ds) != 4 {
			t.Fatalf("expected 4 downstreams, got %d", len(ds))
		}
	})

	var createdRuleID string
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
		if resp.StatusCode != 201 {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
		var out map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&out)
		createdRuleID, _ = out["id"].(string)
	})

	t.Run("UpdateRule", func(t *testing.T) {
		data, _ := json.Marshal(map[string]interface{}{"match_downstreams": []string{"anthropic-sonnet"}})
		req, _ := http.NewRequest(http.MethodPut, apiBase+"/api/rules/default", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("update rule: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("ListPlugins", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/plugins")
		if err != nil {
			t.Fatalf("list plugins: %v", err)
		}
		defer resp.Body.Close()
		var plugins []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&plugins); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		want := []string{"openai2gemini", "anthropic2gemini", "gemini2openai", "gemini2anthropic"}
		have := make(map[string]bool, len(plugins))
		for _, p := range plugins {
			if id, ok := p["id"].(string); ok {
				have[id] = true
			}
		}
		for _, id := range want {
			if !have[id] {
				t.Fatalf("plugin %q not registered", id)
			}
		}
	})

	t.Run("ListAliasGroups", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/aliases")
		if err != nil {
			t.Fatalf("list aliases: %v", err)
		}
		defer resp.Body.Close()
		var groups []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(groups) != 1 || groups[0]["input_model_id"] != "gpt-4o" {
			t.Fatalf("unexpected groups: %+v", groups)
		}
		if opts, _ := groups[0]["options"].([]interface{}); len(opts) != 2 {
			t.Fatalf("expected 2 options, got %d", len(opts))
		}
	})

	t.Run("ActivateAlias", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, apiBase+"/api/aliases/alias-gpt4o-anthropic/activate", bytes.NewReader([]byte("{}")))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("activate alias: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("AddModel", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"model_id": "gpt-4.5-preview"})
		resp, err := client.Post(apiBase+"/api/downstreams/openai-gpt4o/models", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("add model: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("RemoveModel", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, apiBase+"/api/downstreams/openai-gpt4o/models/gpt-4.5-preview", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("remove model: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("DeleteRule", func(t *testing.T) {
		if createdRuleID == "" {
			t.Skip("CreateRule didn't return an id")
		}
		req, _ := http.NewRequest(http.MethodDelete, apiBase+"/api/rules/"+createdRuleID, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("delete rule: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("ProxyRouting", func(t *testing.T) {
		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
		resp, err := client.Post(apiBase+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Logf("expected connect failure (no real upstream): %v", err)
			return
		}
		defer resp.Body.Close()
		t.Logf("proxy status: %d", resp.StatusCode)
	})

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
	})

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
		}
	})

	t.Run("IconEndpoint_UnknownModel404", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/icons/totally-unknown-model-xyz-12345")
		if err != nil {
			t.Fatalf("icon: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("expected 404 for unknown model, got %d", resp.StatusCode)
		}
	})

	if testing.Short() {
		return
	}
	for _, tc := range []struct{ name, path string; wantBytes []byte }{
		{"KnownModel", "/api/icons/gpt-4o", []byte("<?xml")},
		{"FirstSegmentFallback", "/api/icons/MiniMax-M2.5", nil}, // 200 or 404 both OK
	} {
		t.Run("IconEndpoint_"+tc.name, func(t *testing.T) {
			resp, err := client.Get(apiBase + tc.path)
			if err != nil {
				t.Fatalf("icon: %v", err)
			}
			defer resp.Body.Close()
			if tc.wantBytes == nil {
				if resp.StatusCode != 200 && resp.StatusCode != 404 {
					t.Fatalf("expected 200 or 404, got %d", resp.StatusCode)
				}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("expected 200, got %d (body=%q)", resp.StatusCode, body)
			}
			if !bytes.HasPrefix(body, tc.wantBytes) && !bytes.HasPrefix(body, []byte("<svg")) {
				t.Fatalf("expected SVG content, got %q", body[:min(50, len(body))])
			}
			if ct := resp.Header.Get("Content-Type"); !bytes.HasPrefix([]byte(ct), []byte("image/svg")) {
				t.Fatalf("expected image/svg+xml, got %q", ct)
			}
		})
	}
}