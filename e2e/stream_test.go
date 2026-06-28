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

const tresorPort = 9199

// TestAnthropicToOpenAIStreaming verifies that auto-translation from
// Anthropic Messages format to OpenAI Chat Completion format works
// correctly for streaming (SSE) responses.
//
// Scenario: Client sends Anthropic-format streaming request to Tresor.
// Tresor auto-translates to OpenAI format, forwards to downstream, receives
// OpenAI SSE response, and transforms it back to Anthropic SSE format for
// the client.
func TestAnthropicToOpenAIStreaming(t *testing.T) {
	// --- Step 1: Start a mock OpenAI server ---
	mockPort := 9280
	mockServer := startMockOpenAIServer(t, mockPort)
	defer mockServer.Close()

	// --- Step 2: Configure Tresor with the mock downstream ---
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-stream-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Downstream is OpenAI-format, no alias configured.
	// Auto-translation should kick in: Anthropic input → OpenAI downstream.
	cfgContent := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-openai
    name: Mock OpenAI
    api_formats: [openai]
    base_url: http://127.0.0.1:%d/v1
    api_key: sk-mock-key
    output_model_ids:
      - gpt-4o
      - gpt-4o-mini
`, tresorPort, dbPath, mockPort)

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// --- Step 3: Start Tresor daemon ---
	binary, err := filepath.Abs("../tresor.exe")
	if err != nil {
		t.Fatalf("Failed to resolve binary path: %v", err)
	}

	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for Tresor to start
	time.Sleep(2 * time.Second)

	// Health check
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", tresorPort))
	if err != nil || resp.StatusCode != 200 {
		cmd.Process.Kill()
		t.Fatalf("Tresor not ready: err=%v, status=%d", err, safeStatus(resp))
	}
	resp.Body.Close()

	// --- Step 4: Send Anthropic-format streaming request ---
	// The request uses Anthropic Messages format (/v1/messages path)
	// with stream: true. Auto-translation should convert it to OpenAI format.
	anthropicReq := map[string]interface{}{
		"model":      "gpt-4o",
		"max_tokens": 100,
		"stream":     true,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Say hello briefly"},
		},
	}
	body, _ := json.Marshal(anthropicReq)

	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/messages", tresorPort),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("Response status: %d, content-type: %s", resp.StatusCode, resp.Header.Get("Content-Type"))

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}

	// --- Step 5: Parse the SSE stream and verify Anthropic format ---
	contentType := resp.Header.Get("Content-Type")
	if !strings.EqualFold(contentType, "text/event-stream") {
		t.Fatalf("Expected text/event-stream, got %s", contentType)
	}

	// Parse SSE events from the response
	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	var event string
	var data strings.Builder

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			// End of SSE event
			if data.Len() > 0 {
				events = append(events, sseEvent{Type: event, Data: data.String()})
				data.Reset()
			}
			event = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	// Flush last event
	if data.Len() > 0 {
		events = append(events, sseEvent{Type: event, Data: data.String()})
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("Received %d SSE events", len(events))
	for i, e := range events {
		t.Logf("  Event %d: type=%q, data=%s", i, e.Type, e.Data)
	}

	// Verify the event sequence matches Anthropic SSE protocol
	if len(events) == 0 {
		t.Fatal("Expected at least 1 SSE event, got 0")
	}

	// Collect typed events (skip passthrough lines with no event type)
	var typed []sseEvent
	for _, e := range events {
		if e.Type != "" {
			typed = append(typed, e)
		}
	}

	if len(typed) == 0 {
		t.Fatal("Expected at least 1 typed SSE event, got 0")
	}

	// Verify required event types are present
	msgStart := findEvent(typed, "message_start")
	if msgStart == nil {
		t.Fatal("message_start event not found")
	}

	contentStart := findEvent(typed, "content_block_start")
	if contentStart == nil {
		t.Fatal("content_block_start event not found")
	}

	deltas := findEvents(typed, "content_block_delta")
	if len(deltas) == 0 {
		t.Fatal("content_block_delta events not found (expected at least 1)")
	}

	msgDelta := findEvent(typed, "message_delta")
	if msgDelta == nil {
		t.Fatal("message_delta event not found")
	}

	msgStop := findEvent(typed, "message_stop")
	if msgStop == nil {
		t.Fatal("message_stop event not found")
	}

	// Verify ordering by finding indices in typed slice
	idxOf := func(eventType string) int {
		for i, e := range typed {
			if e.Type == eventType {
				return i
			}
		}
		return -1
	}
	iMsgStart := idxOf("message_start")
	iContentStart := idxOf("content_block_start")
	iFirstDelta := idxOf("content_block_delta")
	iMsgDelta := idxOf("message_delta")
	iMsgStop := idxOf("message_stop")
	if iMsgStart >= iContentStart {
		t.Error("message_start should come before content_block_start")
	}
	if iContentStart >= iFirstDelta {
		t.Error("content_block_start should come before first content_block_delta")
	}
	if iFirstDelta >= iMsgDelta {
		t.Error("first content_block_delta should come before message_delta")
	}
	if iMsgDelta >= iMsgStop {
		t.Error("message_delta should come before message_stop")
	}

	// Verify message_start contains valid message structure
	var msgStartData struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(msgStart.Data), &msgStartData); err != nil {
		t.Errorf("Failed to parse message_start data: %v", err)
	} else {
		if msgStartData.Type != "message_start" {
			t.Errorf("message_start type mismatch: got %q", msgStartData.Type)
		}
		if msgStartData.Message.ID == "" {
			t.Error("message_start missing message ID")
		}
		t.Logf("message_start: id=%s, model=%s", msgStartData.Message.ID, msgStartData.Message.Model)
	}

	// Verify content_block_delta contains text
	var totalText strings.Builder
	for _, d := range deltas {
		var deltaData struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(d.Data), &deltaData); err != nil {
			t.Errorf("Failed to parse content_block_delta: %v", err)
		} else if deltaData.Delta.Type == "text_delta" {
			totalText.WriteString(deltaData.Delta.Text)
		}
	}
	text := totalText.String()
	if text == "" {
		t.Error("content_block_delta events contained no text")
	} else {
		t.Logf("Total streamed text: %q", text)
	}

	// Verify message_delta stop_reason
	var msgDeltaData struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason   string `json:"stop_reason"`
			StopSequence string `json:"stop_sequence"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(msgDelta.Data), &msgDeltaData); err != nil {
		t.Errorf("Failed to parse message_delta data: %v", err)
	} else {
		if msgDeltaData.Delta.StopReason == "" {
			t.Error("message_delta missing stop_reason")
		} else {
			t.Logf("message_delta: stop_reason=%s", msgDeltaData.Delta.StopReason)
		}
	}

	t.Log("=== Anthropic→OpenAI streaming auto-translation verified ===")
}

type sseEvent struct {
	Type string
	Data string
}

func findEvent(events []sseEvent, eventType string) *sseEvent {
	for i, e := range events {
		if e.Type == eventType {
			return &events[i]
		}
	}
	return nil
}

func findEvents(events []sseEvent, eventType string) []sseEvent {
	var result []sseEvent
	for _, e := range events {
		if e.Type == eventType {
			result = append(result, e)
		}
	}
	return result
}

// TestOpenAIResponsesStreaming verifies that auto-translation from
// OpenAI Responses API format to Chat Completions format works correctly
// for streaming (SSE) responses, and that no duplicate termination events
// are emitted (the "text part ... not found" bug).
//
// Scenario: Client sends Responses API streaming request to Tresor.
// Tresor auto-translates to Chat Completions format, sends to downstream,
// receives OpenAI Chat Completions SSE, transforms back to Responses API SSE.
func TestOpenAIResponsesStreaming(t *testing.T) {
	// --- Step 1: Start a mock OpenAI Chat Completions server ---
	mockPort := 9290
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		respID := "chatcmpl-responses-test"
		model := "gpt-4o"

		// Emit content chunks
		words := []string{"Hello", " ", "world", "!"}
		for i, word := range words {
			delta := map[string]string{"content": word}
			if i == 0 {
				delta = map[string]string{"role": "assistant", "content": word}
			}
			chunk := map[string]interface{}{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"model":   model,
				"choices": []map[string]interface{}{
					{"index": 0, "delta": delta},
				},
			}
			writeMockSSE(w, chunk)
			flusher.Flush()
		}

		// Emit finish chunk (this triggers termination events)
		finishReason := "stop"
		chunkLast := map[string]interface{}{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{}, "finish_reason": finishReason},
			},
		}
		writeMockSSE(w, chunkLast)
		flusher.Flush()

		// Emit [DONE] marker (this used to cause duplicate termination)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", mockPort), Handler: mux}
	go server.ListenAndServe()
	defer server.Close()
	waitForPort(t, mockPort)

	// --- Step 2: Configure Tresor ---
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-resp-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Downstream with openai format (not openai_responses) → auto-translation
	// should insert Responses2OpenAI plugin.
	cfgContent := fmt.Sprintf(`
bind_addr: 127.0.0.1:%d
db_path: %s

downstreams:
  - id: mock-chat
    name: Mock Chat
    api_formats: [openai]
    base_url: http://127.0.0.1:%d/v1
    api_key: sk-mock-key
    output_model_ids:
      - gpt-4o
`, tresorPort, dbPath, mockPort)

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// --- Step 3: Start Tresor daemon ---
	binary, err := filepath.Abs("../tresor.exe")
	if err != nil {
		t.Fatalf("Failed to resolve binary path: %v", err)
	}

	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}
	defer cmd.Process.Kill()

	time.Sleep(2 * time.Second)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", tresorPort))
	if err != nil || resp.StatusCode != 200 {
		cmd.Process.Kill()
		t.Fatalf("Tresor not ready: err=%v, status=%d", err, safeStatus(resp))
	}
	resp.Body.Close()

	// --- Step 4: Send Responses API streaming request ---
	reqBody := map[string]interface{}{
		"model":      "gpt-4o",
		"input":      "Say hello briefly",
		"stream":     true,
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/responses", tresorPort),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("Response status: %d", resp.StatusCode)
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, bodyBytes)
	}

	// --- Step 5: Parse SSE and verify Responses API format ---
	scanner := bufio.NewScanner(resp.Body)
	var events []sseEvent
	var curEvent string
	var curData strings.Builder

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if curData.Len() > 0 {
				events = append(events, sseEvent{Type: curEvent, Data: curData.String()})
				curData.Reset()
			}
			curEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			curData.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if curData.Len() > 0 {
		events = append(events, sseEvent{Type: curEvent, Data: curData.String()})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("Received %d SSE events", len(events))
	for i, e := range events {
		t.Logf("  Event %d: type=%q, data=%s", i, e.Type, truncate(e.Data, 80))
	}

	if len(events) == 0 {
		t.Fatal("Expected at least 1 SSE event, got 0")
	}

	// Collect typed events (skip empty event: lines)
	var typed []sseEvent
	for _, e := range events {
		if e.Type != "" {
			typed = append(typed, e)
		}
	}

	// Verify Responses API event sequence
	eventTypes := make([]string, len(typed))
	for i, e := range typed {
		eventTypes[i] = e.Type
	}
	t.Logf("Typed events: %v", eventTypes)

	// Must have required events
	requireEvents := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	for _, required := range requireEvents {
		if findEvent(typed, required) == nil {
			t.Errorf("Required event %q not found in stream", required)
		}
	}

	// CRITICAL: Count duplicates — there must be exactly 1 of each
	// termination event (output_item.done and completed).
	// Duplicates cause the "text part ... not found" error in AI SDK.
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

	t.Log("=== OpenAI Responses streaming auto-translation verified ===")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-timeout:
			t.Fatalf("Port :%d not ready", port)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func safeStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// startMockOpenAIServer creates a mock OpenAI chat completions server
// that responds with streaming SSE.
func startMockOpenAIServer(t *testing.T, port int) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Read and verify the request is in OpenAI format
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

		t.Logf("Mock OpenAI received: model=%s, stream=%v, messages=%d", req.Model, req.Stream, len(req.Messages))

		// Verify the request was auto-translated to OpenAI format
		if req.Model == "" {
			t.Error("Mock OpenAI: model was empty in forwarded request")
		}

		// Respond with streaming SSE (OpenAI format)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Emit initial chunk with role
		chunk1 := map[string]interface{}{
			"id":      "chatcmpl-test001",
			"object":  "chat.completion.chunk",
			"model":   req.Model,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant", "content": ""}},
			},
		}
		writeMockSSE(w, chunk1)
		flusher.Flush()

		// Emit content chunks
		words := []string{"Hello", " ", "world", "!", " ", "How", " ", "can", " ", "I", " ", "help", " ", "you", "?"}
		for _, word := range words {
			chunk := map[string]interface{}{
				"id":      "chatcmpl-test001",
				"object":  "chat.completion.chunk",
				"model":   req.Model,
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]string{"content": word}},
				},
			}
			writeMockSSE(w, chunk)
			flusher.Flush()
		}

		// Emit finish chunk
		finishReason := "stop"
		chunkLast := map[string]interface{}{
			"id":      "chatcmpl-test001",
			"object":  "chat.completion.chunk",
			"model":   req.Model,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{}, "finish_reason": finishReason},
			},
		}
		writeMockSSE(w, chunkLast)
		flusher.Flush()

		// Emit [DONE] marker
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

		t.Log("Mock OpenAI: stream completed")
	})

	// Also handle /v1/models endpoint
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"id": "gpt-4o", "object": "model"}},
		})
	})

	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux}
	go server.ListenAndServe()

	// Wait for server to be ready
	timeout := time.After(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			conn.Close()
			t.Logf("Mock OpenAI server listening on :%d", port)
			return server
		}
		select {
		case <-timeout:
			t.Fatal("Mock OpenAI server failed to start")
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func writeMockSSE(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
}

// TestOpenAIToAnthropicStreaming_ThinkingBlocks is a regression test for the
// "empty response" bug observed when proxying an OpenAI client to an
// Anthropic-format reasoning model (e.g. MiniMax-M2.5, DeepSeek-R1).
//
// These models emit a `type: "thinking"` content block with chain-of-thought
// before the visible `type: "text"` block. The OpenAI client must see the
// reasoning surfaced as `delta.reasoning_content` and the visible text as
// `delta.content`. Without the fix, the thinking block is silently dropped
// and (with tight max_tokens) the response is empty.
func TestOpenAIToAnthropicStreaming_ThinkingBlocks(t *testing.T) {
	// --- Step 1: Start a mock Anthropic-format downstream ---
	mockPort := 9300
	mockServer := startMockAnthropicThinkingServer(t, mockPort)
	defer mockServer.Close()

	// --- Step 2: Configure Tresor ---
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-thinking-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	cfgContent := fmt.Sprintf(`
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
`, tresorPort, dbPath, mockPort)

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// --- Step 3: Start Tresor daemon ---
	binary, err := filepath.Abs("../tresor.exe")
	if err != nil {
		t.Fatalf("Failed to resolve binary path: %v", err)
	}

	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}
	defer cmd.Process.Kill()
	time.Sleep(2 * time.Second)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", tresorPort))
	if err != nil || resp.StatusCode != 200 {
		cmd.Process.Kill()
		t.Fatalf("Tresor not ready: err=%v, status=%d", err, safeStatus(resp))
	}
	resp.Body.Close()

	// --- Step 4: Send OpenAI-format streaming request to /v1/chat/completions ---
	openAIReq := map[string]interface{}{
		"model":      "thinking-model",
		"max_tokens": 1000,
		"stream":     true,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Reply with PONG"},
		},
	}
	body, _ := json.Marshal(openAIReq)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", tresorPort),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
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

	// --- Step 5: Parse the OpenAI SSE stream and verify both surfaces ---
	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			events = append(events, sseEvent{Type: "done", Data: payload})
			continue
		}
		events = append(events, sseEvent{Type: "data", Data: payload})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("Received %d SSE events from Tresor", len(events))
	var sawReasoningContent bool
	var sawVisibleText bool
	for _, e := range events {
		if e.Type == "done" {
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
		if err := json.Unmarshal([]byte(e.Data), &chunk); err != nil {
			t.Errorf("Failed to parse chunk: %v, data=%s", err, e.Data)
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.ReasoningContent != "" {
			sawReasoningContent = true
			t.Logf("reasoning_content chunk: %q", d.ReasoningContent)
		}
		if d.Content != "" {
			sawVisibleText = true
			t.Logf("content chunk: %q", d.Content)
		}
	}

	if !sawReasoningContent {
		t.Fatal("expected at least one chunk with delta.reasoning_content, got none")
	}
	if !sawVisibleText {
		t.Fatal("expected at least one chunk with delta.content, got none (the original bug)")
	}
}

// TestResponsesToAnthropicStreaming_ThinkingBlocks is a regression test for
// the "empty response" bug observed when an OpenAI Responses client
// (e.g. openai-python Responses SDK, Cherry Studio) targets an Anthropic-format
// reasoning model (MiniMax-M2.5, DeepSeek-R1). The model emits a `thinking`
// content block before the visible text block, and the Responses SDK requires
// the gateway to surface it as a `type: reasoning` output item at
// output_index 0 (followed by the message item at output_index 1) — otherwise
// the SDK returns an empty response.
func TestResponsesToAnthropicStreaming_ThinkingBlocks(t *testing.T) {
	// --- Step 1: Start a mock Anthropic-format downstream ---
	mockPort := 9301
	mockServer := startMockAnthropicThinkingServer(t, mockPort)
	defer mockServer.Close()

	// --- Step 2: Configure Tresor ---
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-responses-thinking-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	cfgContent := fmt.Sprintf(`
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
`, tresorPort, dbPath, mockPort)

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// --- Step 3: Start Tresor daemon ---
	binary, err := filepath.Abs("../tresor.exe")
	if err != nil {
		t.Fatalf("Failed to resolve binary path: %v", err)
	}

	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}
	defer cmd.Process.Kill()
	time.Sleep(2 * time.Second)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", tresorPort))
	if err != nil || resp.StatusCode != 200 {
		cmd.Process.Kill()
		t.Fatalf("Tresor not ready: err=%v, status=%d", err, safeStatus(resp))
	}
	resp.Body.Close()

	// --- Step 4: Send Responses API streaming request ---
	openAIReq := map[string]interface{}{
		"model":  "thinking-model",
		"input":  "Reply with PONG",
		"stream": true,
		"reasoning": map[string]interface{}{
			"effort": "high",
		},
	}
	body, _ := json.Marshal(openAIReq)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/responses", tresorPort),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
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

	// --- Step 5: Parse the Responses SSE stream and verify both items appear ---
	var events []responsesEvent
	curType := ""
	curData := strings.Builder{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if curData.Len() > 0 {
				events = append(events, responsesEvent{Type: curType, Data: curData.String()})
				curData.Reset()
			}
			curType = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			curType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			curData.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}
	t.Logf("Received %d SSE events from Tresor", len(events))

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
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
				t.Errorf("Bad output_item.added data: %v data=%s", err, e.Data)
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
				t.Errorf("Bad output_item.done data: %v", err)
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

	if !sawReasoningAdd {
		t.Fatal("expected response.output_item.added for a reasoning item (the original bug)")
	}
	if !sawReasoningDelta {
		t.Fatal("expected at least one response.reasoning_summary_text.delta event")
	}
	if !sawReasoningDone {
		t.Fatal("expected response.output_item.done for the reasoning item")
	}
	if !sawMessageAdd {
		t.Fatal("expected response.output_item.added for a message item")
	}
	if !sawTextDelta {
		t.Fatal("expected at least one response.output_text.delta event (the original bug)")
	}
	if !sawMessageDone {
		t.Fatal("expected response.output_item.done for the message item")
	}
	if !sawCompleted {
		t.Fatal("expected response.completed event")
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

// responsesEvent is a typed SSE event (event line + data line) used by the
// Responses API streaming tests.
type responsesEvent struct {
	Type string
	Data string
}

// startMockAnthropicThinkingServer creates a mock downstream that speaks
// the Anthropic Messages SSE protocol and emits a thinking block followed
// by a text block — the pattern produced by reasoning models like
// MiniMax-M2.5 and DeepSeek-R1.
func startMockAnthropicThinkingServer(t *testing.T, port int) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		t.Logf("Mock Anthropic received: model=%v, stream=%v", req["model"], req["stream"])

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// message_start
		writeMockSSEEvent(w, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":    "msg_mock_thinking",
				"model": "thinking-model",
				"role":  "assistant",
			},
		})
		flusher.Flush()

		// content_block_start: thinking block
		writeMockSSEEvent(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]interface{}{
				"type":     "thinking",
				"thinking": "The user wants PONG. I will reply with exactly PONG.",
			},
		})
		flusher.Flush()

		// content_block_stop: thinking block
		writeMockSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
		flusher.Flush()

		// content_block_start: text block (empty initial text)
		writeMockSSEEvent(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": 1,
			"content_block": map[string]interface{}{
				"type": "text",
				"text": "",
			},
		})
		flusher.Flush()

		// content_block_delta: text
		writeMockSSEEvent(w, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 1,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": "PONG",
			},
		})
		flusher.Flush()

		// content_block_stop: text block
		writeMockSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 1,
		})
		flusher.Flush()

		// message_delta: stop_reason
		writeMockSSEEvent(w, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
		})
		flusher.Flush()

		// message_stop
		writeMockSSEEvent(w, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		flusher.Flush()
	})

	// Bind explicitly so the test can fail fast if the port is taken.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Mock server failed to bind 127.0.0.1:%d: %v", port, err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return server
}

// writeMockSSEEvent writes a single typed SSE event (event: ...\ndata: ...\n\n).
func writeMockSSEEvent(w http.ResponseWriter, eventType string, payload interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(data))
}
