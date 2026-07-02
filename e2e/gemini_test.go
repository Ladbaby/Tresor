//go:build integration

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startMockGeminiServer creates a mock Google Gemini server that responds
// to /v1beta/models/{model}:generateContent with a JSON body and
// to :streamGenerateContent?alt=sse with streaming SSE.
//
// The mock verifies the request was sent in Gemini format (contents[]/parts[])
// and the x-goog-api-key header was set.
func startMockGeminiServer(t *testing.T, port int) *http.Server {
	mux := http.NewServeMux()

	// Listing endpoint. Register both bare /models and /v1beta/models
	// because some clients and tools hit either.
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-goog-api-key-echo", r.Header.Get("x-goog-api-key"))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "models/gemini-2.5-pro"},
				{"name": "models/gemini-2.5-flash"},
				{"name": "models/gemini-1.5-pro"},
			},
		})
	})
	mux.HandleFunc("/v1beta/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-goog-api-key-echo", r.Header.Get("x-goog-api-key"))
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
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"role": "model",
						"parts": []map[string]interface{}{
							{"text": "Hello from Gemini!"},
						},
					},
					"finishReason": "STOP",
					"index":        0,
				},
			},
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
		if !strings.Contains(r.URL.RawQuery, "alt=sse") {
			t.Errorf("expected alt=sse query param, got %q", r.URL.RawQuery)
		}
		verifyGeminiRequest(t, r)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		writeGeminiSSE(w, map[string]interface{}{
			"candidates": []map[string]interface{}{{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": []map[string]interface{}{{"text": "Hello"}},
				},
				"index": 0,
			}},
			"modelVersion": "gemini-2.5-pro",
		})
		flusher.Flush()
		writeGeminiSSE(w, map[string]interface{}{
			"candidates": []map[string]interface{}{{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": []map[string]interface{}{{"text": " from Gemini!"}},
				},
				"index": 0,
			}},
		})
		flusher.Flush()
		writeGeminiSSE(w, map[string]interface{}{
			"candidates": []map[string]interface{}{{
				"content":       map[string]interface{}{"role": "model", "parts": []map[string]interface{}{}},
				"finishReason":  "STOP",
				"index":         0,
			}},
			"usageMetadata": map[string]interface{}{"candidatesTokenCount": 5},
		})
		flusher.Flush()
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen on %d: %v", port, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	return srv
}

// verifyGeminiRequest checks that the request is a well-formed Gemini body
// (has contents[]) and was authenticated with x-goog-api-key. systemInstruction
// is optional.
func verifyGeminiRequest(t *testing.T, r *http.Request) {
	if r.Header.Get("x-goog-api-key") == "" {
		t.Errorf("expected x-goog-api-key header, got none")
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode gemini body: %v", err)
		return
	}
	if _, ok := body["contents"]; !ok {
		t.Errorf("expected contents[] in Gemini request, got keys: %v", keysOf(body))
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeGeminiSSE(w io.Writer, payload interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// TestGeminiE2E runs an end-to-end test that exercises the Gemini plugin
// pipeline against a live daemon and a mock Gemini server. It verifies:
//   - The /v1beta/models listing endpoint works (with x-goog-api-key auth).
//   - An OpenAI Chat Completion request is auto-translated to Gemini format
//     and forwarded to the mock Gemini server, which receives a properly
//     shaped Gemini body.
//   - A streaming request goes through the :streamGenerateContent endpoint
//     with alt=sse and the response is correctly translated back to OpenAI
//     SSE chunks.
//   - x-goog-api-key is injected when the downstream's api_formats contains
//     "gemini".
func TestGeminiE2E(t *testing.T) {
	// --- 1. Start mock Gemini server on its own port. ---
	mockPort := 9380
	mockSrv := startMockGeminiServer(t, mockPort)
	defer mockSrv.Close()

	// --- 2. Configure Tresor with the mock Gemini downstream. ---
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-gemini-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Use a different port to avoid clashing with the main TestE2E daemon.
	geminiPort := 9198
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-gemini
    name: Mock Gemini
    api_formats: [gemini]
    base_url: http://127.0.0.1:%d
    api_key: gem-test-key
    output_model_ids:
      - gemini-2.5-pro
`, geminiPort, dbPath, mockPort)

	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// --- 3. Start the daemon. ---
	binary, _ := filepath.Abs("../tresor.exe")
	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	time.Sleep(2 * time.Second)

	apiBase := fmt.Sprintf("http://127.0.0.1:%d", geminiPort)
	client := &http.Client{Timeout: 10 * time.Second}

	// --- 4. Health check. ---
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

	// --- 5. Plugins list includes the new Gemini transformers. ---
	t.Run("PluginsListIncludesGemini", func(t *testing.T) {
		resp, err := client.Get(apiBase + "/api/plugins")
		if err != nil {
			t.Fatalf("plugins: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var plugins []map[string]interface{}
		if err := json.Unmarshal(body, &plugins); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := map[string]bool{
			"openai2gemini":      false,
			"anthropic2gemini":   false,
			"gemini2openai":      false,
			"gemini2anthropic":   false,
		}
		for _, p := range plugins {
			if _, ok := want[p["id"].(string)]; ok {
				want[p["id"].(string)] = true
			}
		}
		for id, found := range want {
			if !found {
				t.Errorf("plugin %s not found in /api/plugins response", id)
			}
		}
	})

	// --- 6. Create a Gemini-format downstream via admin API and verify the
	//       fetch-models endpoint can hit the mock. ---
	t.Run("CreateGeminiDownstream", func(t *testing.T) {
		body := map[string]interface{}{
			"id":         "test-gemini",
			"name":       "Test Gemini",
			"api_formats": []string{"gemini"},
			"base_url":   fmt.Sprintf("http://127.0.0.1:%d", mockPort),
			"api_key":    "gem-test-key",
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
		t.Logf("Fetched models: %v", out.ModelIDs)
	})

	// --- 7. OpenAI → Gemini auto-translation. ---
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
		choices, _ := oaiResp["choices"].([]interface{})
		if len(choices) == 0 {
			t.Fatalf("expected 1 choice, got %d (body: %s)", len(choices), respBody)
		}
		choice := choices[0].(map[string]interface{})
		msg := choice["message"].(map[string]interface{})
		if msg["content"] != "Hello from Gemini!" {
			t.Fatalf("expected content 'Hello from Gemini!', got %v", msg["content"])
		}
		if choice["finish_reason"] != "stop" {
			t.Fatalf("expected finish_reason 'stop', got %v", choice["finish_reason"])
		}
		usage := oaiResp["usage"].(map[string]interface{})
		if usage["total_tokens"] != float64(8) {
			t.Fatalf("expected total_tokens 8, got %v", usage["total_tokens"])
		}
	})

	// --- 8. Streaming: OpenAI → Gemini SSE → OpenAI SSE. ---
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
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("expected text/event-stream, got %q", resp.Header.Get("Content-Type"))
		}
		// Read chunks; verify we get at least one content delta and a [DONE] marker.
		var gotContent, gotDone bool
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			if line == "" {
				continue
			}
			// Some servers emit "[DONE]" with no "data: " prefix; handle both.
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				gotDone = true
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
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
			c0, _ := choices[0].(map[string]interface{})
			delta, _ := c0["delta"].(map[string]interface{})
			if c, ok := delta["content"].(string); ok && c != "" {
				gotContent = true
			}
		}
		if !gotContent {
			t.Fatal("expected at least one content delta in streaming response")
		}
		if !gotDone {
			t.Fatal("expected [DONE] marker in streaming response")
		}
	})

	// --- 9. Anthropic → Gemini auto-translation. ---
	t.Run("AnthropicToGemini_AutoTranslate", func(t *testing.T) {
		body := map[string]interface{}{
			"model":      "gemini-2.5-pro",
			"max_tokens": 100,
			"system":     "You are helpful.",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Hi"},
			},
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
		content, _ := anth["content"].([]interface{})
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
