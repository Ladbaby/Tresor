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

// openAIChunk represents an OpenAI chat completion streaming chunk.
//
// It is used both as a parser (responses2openai, anthropic2openai,
// gemini2openai read upstream chunks through it) and as an encoder
// (openai2anthropic, openai2gemini build outgoing chunks from it). Every field
// beyond the original four is a pointer or `omitempty` so that encoder output is
// byte-identical when the field is unset.
type openAIChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Created int64              `json:"created,omitempty"`
	Choices []openAIChunkChoice `json:"choices"`

	// SystemFingerprint and ServiceTier are upstream response metadata that the
	// Chat Completions spec allows on chunks. They are carried through instead of
	// being dropped so a converted response keeps the upstream's identity.
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
	ServiceTier       string `json:"service_tier,omitempty"`

	// Usage is the token accounting block. Upstreams send it either on the
	// finish_reason chunk (llama.cpp, vLLM) or on a trailing chunk with empty
	// choices (OpenAI's include_usage mode), so parsers must look at every chunk.
	Usage *openAIChunkUsage `json:"usage,omitempty"`

	// Timings is not part of any OpenAI spec. llama.cpp and vLLM attach it next
	// to usage to report decode throughput (predicted_per_second, draft_n, ...).
	// It is passed through verbatim for clients that want the number; deleting
	// this field reverts to dropping it.
	Timings map[string]any `json:"timings,omitempty"`
}

// openAIChunkUsage is the Chat Completions token accounting block. The Details
// sub-objects are pointers so "upstream did not report it" stays distinguishable
// from "upstream reported zero" — absent data must not become a fake zero.
type openAIChunkUsage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *openAITokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *openAITokenDetails `json:"completion_tokens_details,omitempty"`
}

// openAITokenDetails carries the nested detail counters that some upstreams
// report alongside the top-level token counts.
type openAITokenDetails struct {
	CachedTokens    *int `json:"cached_tokens,omitempty"`
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

// openAIChunkChoice represents one streaming choice.
type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason,omitempty"`
}

// openAIToolCallDelta represents a tool call in a streaming delta.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// openAIDelta represents the content delta in a streaming choice.
type openAIDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          string                `json:"content,omitempty"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
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
