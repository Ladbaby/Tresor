package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicContentBlock represents a single content block in an Anthropic message.
// Modern Anthropic API uses content blocks (arrays) instead of plain strings.
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// flexibleContent represents message content that can be either a plain string
// or an array of content blocks. UnmarshalJSON handles both formats.
type flexibleContent struct {
	// Text holds the extracted plain-text representation.
	// For string content: the string itself.
	// For array content: concatenated text from all "text" type blocks.
	Text string
}

func (c *flexibleContent) UnmarshalJSON(data []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}

	// Try array of content blocks
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return fmt.Errorf("flexibleContent: invalid format: %w", err)
	}

	// Concatenate all text blocks
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	c.Text = strings.Join(parts, "\n")
	return nil
}

func (c flexibleContent) MarshalJSON() ([]byte, error) {
	// Marshal as plain string (used for system prompt field)
	return json.Marshal(c.Text)
}

// contentBlockArray represents message content that marshals as an Anthropic
// content block array: [{"type":"text","text":"Hello"}].
type contentBlockArray struct {
	Text string
}

func (c *contentBlockArray) UnmarshalJSON(data []byte) error {
	// Reuse flexibleContent's unmarshal logic
	var fc flexibleContent
	if err := fc.UnmarshalJSON(data); err != nil {
		return err
	}
	c.Text = fc.Text
	return nil
}

func (c contentBlockArray) MarshalJSON() ([]byte, error) {
	// Marshal as Anthropic content block array
	return json.Marshal([]map[string]interface{}{
		{"type": "text", "text": c.Text},
	})
}

// AnthropicMessage represents a message in Anthropic Messages API format.
// Content can be either a plain string or an array of content blocks on input.
// Marshals as a content block array for Anthropic compatibility.
type AnthropicMessage struct {
	Role    string             `json:"role"`
	Content *contentBlockArray `json:"content"`
}

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
