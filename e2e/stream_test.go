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
	"sync/atomic"
	"testing"
	"time"
)

const streamPort = 9200

// mockChatSSE writes an OpenAI-format streaming response with the given words.
func mockChatSSE(w http.ResponseWriter, words []string, model, respID string) {
	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for i, word := range words {
		delta := map[string]string{"content": word}
		if i == 0 {
			delta = map[string]string{"role": "assistant", "content": word}
		}
		chunk := map[string]interface{}{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta}},
		}
		writeMockSSE(w, chunk)
		flusher.Flush()
	}

	finish := map[string]interface{}{
		"id":      respID,
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	}
	writeMockSSE(w, finish)
	flusher.Flush()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeMockSSE(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
}

func writeMockSSEEvent(w http.ResponseWriter, eventType string, payload interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(data))
}

// startMockChatServer: mock OpenAI Chat Completions server with optional
// request body echo-logging. words is the content streamed back.
func startMockChatServer(t *testing.T, port int, words []string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			t.Error("Mock OpenAI: model was empty in forwarded request")
		}
		mockChatSSE(w, words, req.Model, "chatcmpl-test001")
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"id": "gpt-4o", "object": "model"}},
		})
	})
	return startMockServer(t, port, mux)
}

// startMockAnthropicThinkingServer emits a thinking content block followed
// by a visible text block — the shape produced by reasoning models.
func startMockAnthropicThinkingServer(t *testing.T, port int) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		t.Logf("Mock Anthropic received: model=%v, stream=%v", req["model"], req["stream"])
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		events := []struct {
			event string
			data  map[string]interface{}
		}{
			{"message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": "msg_mock_thinking", "model": "thinking-model", "role": "assistant"}}},
			{"content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "thinking", "thinking": "The user wants PONG. I will reply with exactly PONG."}}},
			{"content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0}},
			{"content_block_start", map[string]interface{}{"type": "content_block_start", "index": 1, "content_block": map[string]interface{}{"type": "text", "text": ""}}},
			{"content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 1, "delta": map[string]interface{}{"type": "text_delta", "text": "PONG"}}},
			{"content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 1}},
			{"message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil}}},
			{"message_stop", map[string]interface{}{"type": "message_stop"}},
		}
		for _, e := range events {
			writeMockSSEEvent(w, e.event, e.data)
			flusher.Flush()
		}
	})
	return startMockServer(t, port, mux)
}

func TestAnthropicToOpenAIStreaming(t *testing.T) {
	mockPort := 9280
	mockSrv := startMockChatServer(t, mockPort,
		[]string{"Hello", " ", "world", "!", " ", "How", " ", "can", " ", "I", " ", "help", " ", "you", "?"})
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-openai
    name: Mock OpenAI
    api_formats: [openai]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - gpt-4o
      - gpt-4o-mini
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":      "gpt-4o",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]interface{}{{"role": "user", "content": "Say hello briefly"}},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}
	if !strings.EqualFold(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	events, err := scanSSE(resp.Body)
	if err != nil {
		t.Fatalf("Scanner error: %v", err)
	}
	var typed []sseEvent
	for _, e := range events {
		if e.Type != "" {
			typed = append(typed, e)
		}
	}
	if len(typed) == 0 {
		t.Fatal("Expected at least 1 typed SSE event, got 0")
	}
	for _, et := range []string{"message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop"} {
		if findEvent(typed, et) == nil {
			t.Errorf("missing required event %q", et)
		}
	}
	if deltas := findEvents(typed, "content_block_delta"); len(deltas) == 0 {
		t.Fatal("content_block_delta events not found")
	}
}

func TestOpenAIResponsesStreaming(t *testing.T) {
	mockPort := 9290
	mockSrv := startMockChatServer(t, mockPort, []string{"Hello", " ", "world", "!"})
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-chat
    name: Mock Chat
    api_formats: [openai]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - gpt-4o
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":  "gpt-4o",
		"input":  "Say hello briefly",
		"stream": true,
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}

	events, err := scanSSE(resp.Body)
	if err != nil {
		t.Fatalf("Scanner error: %v", err)
	}
	var typed []sseEvent
	for _, e := range events {
		if e.Type != "" {
			typed = append(typed, e)
		}
	}
	if len(typed) == 0 {
		t.Fatal("Expected at least 1 SSE event, got 0")
	}
	required := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	for _, r := range required {
		if findEvent(typed, r) == nil {
			t.Errorf("Required event %q not found in stream", r)
		}
	}
	// CRITICAL: no duplicate termination events (the "text part ... not found" bug).
	for _, et := range []string{"response.output_item.done", "response.completed"} {
		count := 0
		for _, e := range typed {
			if e.Type == et {
				count++
			}
		}
		if count != 1 {
			t.Errorf("Expected exactly 1 %q event, got %d (duplicate termination bug)", et, count)
		}
	}
}

func TestOpenAIToAnthropicStreaming_ThinkingBlocks(t *testing.T) {
	mockPort := 9300
	mockSrv := startMockAnthropicThinkingServer(t, mockPort)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-anthropic
    name: Mock Anthropic
    api_formats: [anthropic]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - thinking-model
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":      "thinking-model",
		"max_tokens": 1000,
		"stream":     true,
		"messages":   []map[string]interface{}{{"role": "user", "content": "Reply with PONG"}},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.EqualFold(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var sawReasoningContent, sawVisibleText bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Errorf("Failed to parse chunk: %v, data=%s", err, payload)
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.ReasoningContent != "" {
			sawReasoningContent = true
		}
		if d.Content != "" {
			sawVisibleText = true
		}
	}
	if !sawReasoningContent {
		t.Fatal("expected at least one chunk with delta.reasoning_content, got none")
	}
	if !sawVisibleText {
		t.Fatal("expected at least one chunk with delta.content, got none (the original bug)")
	}
}

func TestResponsesToAnthropicStreaming_ThinkingBlocks(t *testing.T) {
	mockPort := 9301
	mockSrv := startMockAnthropicThinkingServer(t, mockPort)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-anthropic
    name: Mock Anthropic
    api_formats: [anthropic]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - thinking-model
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":  "thinking-model",
		"input":  "Reply with PONG",
		"stream": true,
		"reasoning": map[string]interface{}{"effort": "high"},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.EqualFold(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	events, err := scanSSE(resp.Body)
	if err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	var reasoningOI, messageOI int
	var reasoningOIOK, messageOIOK bool
	var sawReasoningAdd, sawMessageAdd bool
	var sawReasoningDelta, sawTextDelta bool
	var sawReasoningDone, sawMessageDone bool
	var sawCompleted bool

	for _, e := range events {
		switch e.Type {
		case "response.output_item.added":
			var ev struct {
				OutputIndex int `json:"output_index"`
				Item        struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
				continue
			}
			switch ev.Item.Type {
			case "reasoning":
				sawReasoningAdd = true
				reasoningOI = ev.OutputIndex
				reasoningOIOK = true
			case "message":
				sawMessageAdd = true
				messageOI = ev.OutputIndex
				messageOIOK = true
			}
		case "response.reasoning_summary_text.delta":
			sawReasoningDelta = true
		case "response.output_text.delta":
			sawTextDelta = true
		case "response.output_item.done":
			var ev struct {
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
				continue
			}
			switch ev.Item.Type {
			case "reasoning":
				sawReasoningDone = true
			case "message":
				sawMessageDone = true
			}
		case "response.completed":
			sawCompleted = true
		}
	}

	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"response.output_item.added for reasoning", sawReasoningAdd},
		{"response.reasoning_summary_text.delta", sawReasoningDelta},
		{"response.output_item.done for reasoning", sawReasoningDone},
		{"response.output_item.added for message", sawMessageAdd},
		{"response.output_text.delta", sawTextDelta},
		{"response.output_item.done for message", sawMessageDone},
		{"response.completed", sawCompleted},
	} {
		if !c.ok {
			t.Errorf("missing %s", c.name)
		}
	}
	if reasoningOIOK && messageOIOK {
		if reasoningOI != 0 {
			t.Fatalf("expected reasoning at output_index 0, got %d", reasoningOI)
		}
		if messageOI != 1 {
			t.Fatalf("expected message at output_index 1, got %d", messageOI)
		}
	}
}

// startMockAnthropicCapServer emits an Anthropic SSE stream whose message_delta
// carries `stop_reason: max_tokens`, mirroring the wire shape captured in
// `incomplete-response-openai-client-anthropic-server*.txt`. The captured
// `max_tokens` value is reported back via `*atomic.Int64` so E2E tests can
// assert that the gateway forwarded the correct cap to the downstream.
func startMockAnthropicCapServer(t *testing.T, port int, captured *atomic.Int64) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if mt, ok := req["max_tokens"].(float64); ok && captured != nil {
			captured.Store(int64(mt))
		}
		t.Logf("Mock Anthropic cap server received: max_tokens=%v", req["max_tokens"])
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		events := []struct {
			event string
			data  map[string]interface{}
		}{
			{"message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": "msg_cap", "model": "cap-model", "role": "assistant"}}},
			{"content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "text", "text": ""}}},
			{"content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "text_delta", "text": "Hi"}}},
			{"content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0}},
			{"message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": "max_tokens", "stop_sequence": nil}, "usage": map[string]interface{}{"output_tokens": 256}}},
			{"message_stop", map[string]interface{}{"type": "message_stop"}},
		}
		for _, e := range events {
			writeMockSSEEvent(w, e.event, e.data)
			flusher.Flush()
		}
	})
	return startMockServer(t, port, mux)
}

// TestOpenAIToAnthropicStreaming_DefaultMaxTokensForwarded verifies that when
// an OpenAI Chat Completions client sends NO token cap, the gateway forwards
// the Anthropic Sonnet 4.5 / Claude Code default of 32000 to the downstream.
// Regression guard for the issue where Tresor injected 1024 and Anthropic
// obediently truncated at that ceiling.
func TestOpenAIToAnthropicStreaming_DefaultMaxTokensForwarded(t *testing.T) {
	mockPort := 9302
	var capturedMaxTokens atomic.Int64
	mockSrv := startMockAnthropicCapServer(t, mockPort, &capturedMaxTokens)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-anthropic
    name: Mock Anthropic
    api_formats: [anthropic]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - cap-model
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	// NO max_tokens / max_completion_tokens: the gateway must default to 32000.
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":    "cap-model",
		"stream":   true,
		"messages": []map[string]interface{}{{"role": "user", "content": "Say hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}
	// Drain the stream so the daemon flushes its request to the mock.
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := capturedMaxTokens.Load(); got != 32000 {
		t.Fatalf("expected downstream max_tokens 32000 (default), got %d", got)
	}
}

// TestOpenAIToAnthropicStreaming_MaxCompletionTokensForwarded verifies that
// the modern Chat Completions field max_completion_tokens is forwarded as
// Anthropic max_tokens. Without this, modern OpenAI SDKs silently hit the
// transformer's default cap and downstreams truncate.
func TestOpenAIToAnthropicStreaming_MaxCompletionTokensForwarded(t *testing.T) {
	mockPort := 9303
	var capturedMaxTokens atomic.Int64
	mockSrv := startMockAnthropicCapServer(t, mockPort, &capturedMaxTokens)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-anthropic
    name: Mock Anthropic
    api_formats: [anthropic]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - cap-model
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":                "cap-model",
		"max_completion_tokens": 1024,
		"stream":               true,
		"messages":             []map[string]interface{}{{"role": "user", "content": "Say hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := capturedMaxTokens.Load(); got != 1024 {
		t.Fatalf("expected downstream max_tokens 1024 (max_completion_tokens), got %d", got)
	}
}

// TestOpenAIToAnthropicStreaming_StopReasonLength verifies that when the
// Anthropic downstream emits `stop_reason: max_tokens`, the OpenAI client sees
// `finish_reason: length` and the terminal [DONE] marker. Regression guard for
// the captured symptom where the stream "appeared stripped" because the
// OpenAI client had no way to detect the cap.
func TestOpenAIToAnthropicStreaming_StopReasonLength(t *testing.T) {
	mockPort := 9304
	mockSrv := startMockAnthropicCapServer(t, mockPort, nil)
	defer mockSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-anthropic
    name: Mock Anthropic
    api_formats: [anthropic]
    base_url: http://127.0.0.1:%d
    api_key: sk-mock-key
    output_model_ids:
      - cap-model
`, streamPort, dbPath, mockPort)
	apiBase, cleanup := startTresor(t, cfg, streamPort)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":      "cap-model",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]interface{}{{"role": "user", "content": "Say hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var sawLength, sawDone bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != nil && *c.FinishReason == "length" {
				sawLength = true
			}
		}
	}
	if !sawLength {
		t.Fatal("expected at least one chunk with finish_reason=length, got none")
	}
	if !sawDone {
		t.Fatal("expected [DONE] marker at end of stream")
	}
}