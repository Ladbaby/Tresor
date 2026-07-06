//go:build integration

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const geminiPort = 9198

func startMockGeminiServer(t *testing.T, port int) *http.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Listing endpoint. Engine calls /v1beta/models.
	mux.HandleFunc("/v1beta/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "models/gemini-2.5-pro"},
				{"name": "models/gemini-2.5-flash"},
				{"name": "models/gemini-1.5-pro"},
			},
		})
	})

	// Generate content (non-streaming).
	mux.HandleFunc("/v1beta/models/gemini-2.5-pro:generateContent", func(w http.ResponseWriter, r *http.Request) {
		verifyGeminiRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"candidates": []map[string]interface{}{{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": []map[string]interface{}{{"text": "Hello from Gemini!"}},
				},
				"finishReason": "STOP",
				"index":        0,
			}},
			"usageMetadata": map[string]interface{}{
				"promptTokenCount":     4,
				"candidatesTokenCount": 4,
				"totalTokenCount":      8,
			},
			"modelVersion": "gemini-2.5-pro",
		})
	})

	// Generate content (streaming).
	mux.HandleFunc("/v1beta/models/gemini-2.5-pro:streamGenerateContent", func(w http.ResponseWriter, r *http.Request) {
		verifyGeminiRequest(t, r)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		for _, payload := range []map[string]interface{}{
			{"candidates": []map[string]interface{}{{"content": map[string]interface{}{"role": "model", "parts": []map[string]interface{}{{"text": "Hello"}}}, "index": 0}}, "modelVersion": "gemini-2.5-pro"},
			{"candidates": []map[string]interface{}{{"content": map[string]interface{}{"role": "model", "parts": []map[string]interface{}{{"text": " from Gemini!"}}}, "index": 0}}},
			{"candidates": []map[string]interface{}{{"content": map[string]interface{}{"role": "model", "parts": []map[string]interface{}{}}, "finishReason": "STOP", "index": 0}}, "usageMetadata": map[string]interface{}{"candidatesTokenCount": 5}},
		} {
			writeGeminiSSE(w, payload)
			flusher.Flush()
		}
	})

	return startMockServer(t, port, mux)
}

func verifyGeminiRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("x-goog-api-key") == "" {
		t.Errorf("expected x-goog-api-key header, got none")
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode gemini body: %v", err)
		return
	}
	if _, ok := body["contents"]; !ok {
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		t.Errorf("expected contents[] in Gemini request, got keys: %v", keys)
	}
}

func writeGeminiSSE(w io.Writer, payload interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func TestGeminiE2E(t *testing.T) {
	mockSrv := startMockGeminiServer(t, 9380)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-gemini
    name: Mock Gemini
    api_formats: [gemini]
    base_url: http://127.0.0.1:9380
    api_key: gem-test-key
    output_model_ids:
      - gemini-2.5-pro
`, geminiPort, dbPath)

	apiBase, cleanup := startTresor(t, cfg, geminiPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}

	t.Run("HealthCheck", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/health")
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("PluginsListIncludesGemini", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/plugins")
		if err != nil {
			t.Fatalf("plugins: %v", err)
		}
		defer resp.Body.Close()
		var plugins []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&plugins); err != nil {
			t.Fatalf("decode: %v", err)
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
				t.Errorf("plugin %s not found in /api/plugins", id)
			}
		}
	})

	t.Run("CreateGeminiDownstream", func(t *testing.T) {
		body := map[string]interface{}{
			"id":              "test-gemini",
			"name":            "Test Gemini",
			"api_formats":     []string{"gemini"},
			"base_url":        "http://127.0.0.1:9380",
			"api_key":         "gem-test-key",
			"output_model_ids": []string{},
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", apiBase+"/api/downstreams", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d (body: %s)", resp.StatusCode, respBody)
		}
	})

	t.Run("FetchGeminiModels", func(t *testing.T) {
		req, _ := http.NewRequest("POST", apiBase+"/api/downstreams/test-gemini/fetch-models", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("fetch-models: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d (body: %s)", resp.StatusCode, respBody)
		}
		var out struct {
			ModelIDs []string `json:"model_ids"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("decode: %v (body: %s)", err, respBody)
		}
		if len(out.ModelIDs) == 0 {
			t.Fatalf("expected model_ids, got none (body: %s)", respBody)
		}
	})

	t.Run("OpenAIToGemini_AutoTranslate", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "gemini-2.5-pro",
			"messages": []map[string]interface{}{
				{"role": "system", "content": "Be helpful."},
				{"role": "user", "content": "Hello"},
			},
			"max_tokens": 100,
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", apiBase+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d (body: %s)", resp.StatusCode, respBody)
		}
		var oaiResp map[string]interface{}
		if err := json.Unmarshal(respBody, &oaiResp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		choices := oaiResp["choices"].([]interface{})
		if len(choices) == 0 {
			t.Fatalf("expected 1 choice, got 0")
		}
		msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
		if msg["content"] != "Hello from Gemini!" {
			t.Fatalf("expected content 'Hello from Gemini!', got %v", msg["content"])
		}
		if reason := choices[0].(map[string]interface{})["finish_reason"]; reason != "stop" {
			t.Fatalf("expected finish_reason 'stop', got %v", reason)
		}
		usage := oaiResp["usage"].(map[string]interface{})
		if usage["total_tokens"] != float64(8) {
			t.Fatalf("expected total_tokens 8, got %v", usage["total_tokens"])
		}
	})

	t.Run("OpenAIToGemini_Streaming", func(t *testing.T) {
		body := map[string]interface{}{
			"model":    "gemini-2.5-pro",
			"messages": []map[string]interface{}{{"role": "user", "content": "Hello"}},
			"stream":   true,
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", apiBase+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		defer resp.Body.Close()
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("expected text/event-stream, got %q", resp.Header.Get("Content-Type"))
		}
		var gotContent, gotDone bool
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				gotDone = true
				continue
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			choices, _ := chunk["choices"].([]interface{})
			if len(choices) == 0 {
				continue
			}
			c0 := choices[0].(map[string]interface{})
			if d, _ := c0["delta"].(map[string]interface{}); d != nil {
				if c, ok := d["content"].(string); ok && c != "" {
					gotContent = true
				}
			}
		}
		if !gotContent {
			t.Fatal("expected at least one content delta in streaming response")
		}
		if !gotDone {
			t.Fatal("expected [DONE] marker in streaming response")
		}
	})

	t.Run("AnthropicToGemini_AutoTranslate", func(t *testing.T) {
		body := map[string]interface{}{
			"model":      "gemini-2.5-pro",
			"max_tokens": 100,
			"system":     "You are helpful.",
			"messages":    []map[string]interface{}{{"role": "user", "content": "Hi"}},
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", apiBase+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "client-key")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("messages: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d (body: %s)", resp.StatusCode, respBody)
		}
		var anth map[string]interface{}
		if err := json.Unmarshal(respBody, &anth); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if anth["role"] != "assistant" {
			t.Fatalf("expected assistant, got %v", anth["role"])
		}
		content := anth["content"].([]interface{})
		if len(content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(content))
		}
		block := content[0].(map[string]interface{})
		if block["type"] != "text" || block["text"] != "Hello from Gemini!" {
			t.Fatalf("unexpected content: %v", block)
		}
		if anth["stop_reason"] != "end_turn" {
			t.Fatalf("expected end_turn, got %v", anth["stop_reason"])
		}
	})
}