package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// openAIChunk represents an OpenAI chat completion streaming chunk.
type openAIChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
}

// openAIChunkChoice represents one streaming choice.
type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason,omitempty"`
}

// openAIDelta represents the content delta in a streaming choice.
type openAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// writeSSEData marshals v as JSON and writes an SSE "data:" line to buf.
func writeSSEData(buf *bytes.Buffer, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}

// writeDoneMarker writes the OpenAI streaming done marker "[DONE]".
func writeDoneMarker(buf *bytes.Buffer) {
	buf.WriteString("data: [DONE]\n\n")
}

// parseAnthropicSSE parses SSE bytes and calls fn for each event.
// fn returns false to stop parsing.
func parseAnthropicSSE(data []byte, fn func(eventType string, data []byte) bool) {
	lines := strings.Split(string(data), "\n")
	var currentEvent string
	var currentData []string

	flush := func() bool {
		if currentEvent == "" {
			return true
		}
		payload := []byte(strings.Join(currentData, "\n"))
		return fn(currentEvent, payload)
	}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if !flush() {
				return
			}
			currentEvent = ""
			currentData = nil
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			currentData = append(currentData, strings.TrimPrefix(line, "data: "))
		}
	}
	// Flush any remaining event
	flush()
}

// parseOpenAISSE parses OpenAI-style SSE lines and calls fn for each data payload.
// fn returns false to stop parsing.
func parseOpenAISSE(data []byte, fn func(data []byte) bool) {
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data: ") {
			payload := []byte(strings.TrimPrefix(line, "data: "))
			payload = bytes.TrimSpace(payload)
			if string(payload) == "[DONE]" {
				break
			}
			if !fn(payload) {
				return
			}
		}
	}
}

// anthropicStreamEvent writes an Anthropic SSE event to buf.
func writeAnthropicSSE(buf *bytes.Buffer, eventType string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(buf, "event: %s\n", eventType)
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
}
