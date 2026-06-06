package plugins

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteSSEData(t *testing.T) {
	var buf bytes.Buffer
	writeSSEData(&buf, map[string]string{"key": "value"})

	expected := "data: {\"key\":\"value\"}\n\n"
	if buf.String() != expected {
		t.Fatalf("expected %q, got %q", expected, buf.String())
	}
}

func TestWriteSSEData_Chunk(t *testing.T) {
	var buf bytes.Buffer
	chunk := openAIChunk{
		ID:    "chatcmpl-123",
		Model: "gpt-4o",
		Choices: []openAIChunkChoice{
			{Index: 0, Delta: openAIDelta{Content: "Hello"}},
		},
	}
	writeSSEData(&buf, chunk)

	if !bytes.Contains(buf.Bytes(), []byte("data: ")) {
		t.Fatal("expected data prefix")
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"id":"chatcmpl-123"`)) {
		t.Fatal("expected id in output")
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"content":"Hello"`)) {
		t.Fatal("expected content in output")
	}
}

func TestWriteDoneMarker(t *testing.T) {
	var buf bytes.Buffer
	writeDoneMarker(&buf)

	expected := "data: [DONE]\n\n"
	if buf.String() != expected {
		t.Fatalf("expected %q, got %q", expected, buf.String())
	}
}

func TestParseOpenAISSE(t *testing.T) {
	sseData := []byte("data: {\"id\":\"1\",\"content\":\"Hello\"}\n\ndata: {\"id\":\"2\",\"content\":\"World\"}\n\ndata: [DONE]\n\n")

	var payloads []string
	parseOpenAISSE(sseData, func(data []byte) bool {
		payloads = append(payloads, string(data))
		return true
	})

	if len(payloads) != 2 {
		t.Fatalf("expected 2 payloads, got %d", len(payloads))
	}

	var p1 map[string]interface{}
	json.Unmarshal([]byte(payloads[0]), &p1)
	if p1["id"] != "1" {
		t.Fatalf("expected id 1, got %v", p1["id"])
	}
}

func TestParseOpenAISSE_EarlyStop(t *testing.T) {
	sseData := []byte("data: chunk1\n\ndata: chunk2\n\ndata: chunk3\n\n")

	count := 0
	parseOpenAISSE(sseData, func(data []byte) bool {
		count++
		return false // stop after first
	})

	if count != 1 {
		t.Fatalf("expected 1 call (early stop), got %d", count)
	}
}

func TestParseAnthropicSSE(t *testing.T) {
	sseData := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\"}\n\n")

	var events []string
	parseAnthropicSSE(sseData, func(eventType string, data []byte) bool {
		events = append(events, eventType)
		return true
	})

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0] != "message_start" {
		t.Fatalf("expected first event 'message_start', got %q", events[0])
	}
	if events[1] != "content_block_start" {
		t.Fatalf("expected second event 'content_block_start', got %q", events[1])
	}
}

func TestParseAnthropicSSE_EarlyStop(t *testing.T) {
	sseData := []byte("event: e1\ndata: d1\n\nevent: e2\ndata: d2\n\n")

	count := 0
	parseAnthropicSSE(sseData, func(eventType string, data []byte) bool {
		count++
		return false // stop after first
	})

	if count != 1 {
		t.Fatalf("expected 1 call (early stop), got %d", count)
	}
}

func TestWriteAnthropicSSE(t *testing.T) {
	var buf bytes.Buffer
	writeAnthropicSSE(&buf, "message_start", map[string]string{"type": "message_start"})

	expected := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	if buf.String() != expected {
		t.Fatalf("expected %q, got %q", expected, buf.String())
	}
}
