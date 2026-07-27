package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Retry constants — mirror Claude Code's backoff pattern.
const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 16 * time.Second
	retryMaxCount  = 3
)

// CalculateBackoff returns the delay to wait before the next retry attempt.
// Uses exponential backoff: BaseDelay * 2^(attempt-1), capped at MaxDelay,
// with random jitter (0-25% of base delay).
//
// Attempt numbering: attempt 1 = first retry, attempt 2 = second retry, etc.
func CalculateBackoff(attempt int) time.Duration {
	base := time.Duration(float64(retryBaseDelay) * math.Pow(2, float64(attempt-1)))
	if base > retryMaxDelay {
		base = retryMaxDelay
	}
	// Jitter: random 0-25% of base delay
	jitter := time.Duration(float64(base) * rand.Float64() * 0.25)
	return base + jitter
}

// ---- bufferedWriter ----
//
// Wraps an http.ResponseWriter to buffer writes until Flush() is called.
// Used by the streaming handler to delay client writes until content is
// confirmed, enabling retry on empty responses.

type bufferedWriter struct {
	writer  http.ResponseWriter
	buffer  *bytes.Buffer
	status  int
	flushed bool
}

func newBufferedWriter(w http.ResponseWriter) *bufferedWriter {
	return &bufferedWriter{
		writer:  w,
		buffer:  &bytes.Buffer{},
		status:  http.StatusOK,
		flushed: false,
	}
}

func (bw *bufferedWriter) Header() http.Header {
	return bw.writer.Header()
}

func (bw *bufferedWriter) Write(p []byte) (int, error) {
	if bw.flushed {
		return bw.writer.Write(p)
	}
	return bw.buffer.Write(p)
}

func (bw *bufferedWriter) WriteHeader(status int) {
	if bw.flushed {
		bw.writer.WriteHeader(status)
	} else {
		bw.status = status
	}
}

// Flush commits the buffered content to the underlying writer and switches
// to pass-through mode. Subsequent writes go directly to the writer.
func (bw *bufferedWriter) Flush() {
	if bw.flushed {
		return
	}
	bw.flushed = true
	bw.writer.WriteHeader(bw.status)
	bw.writer.Write(bw.buffer.Bytes())
	bw.buffer.Reset()
	if f, ok := bw.writer.(http.Flusher); ok {
		f.Flush()
	}
}

// IsFlushed reports whether Flush() has been called. Used to detect empty
// streams: if the writer was never flushed, no content was produced.
func (bw *bufferedWriter) IsFlushed() bool {
	return bw.flushed
}

// ---- Empty response detection (non-streaming) ----
//
// Each function parses the raw upstream response body and returns true if
// the response contains no useful content (no text, no tool calls, etc.).

// IsOpenAIChatEmpty returns true if an OpenAI Chat Completions response
// contains no content. Checks for empty choices array, or choices with
// no text content, tool calls, or refusal. Reasoning/thinking content
// is excluded — thinking-only responses are treated as empty.
func IsOpenAIChatEmpty(body []byte) bool {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   interface{}       `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
				Refusal   string `json:"refusal,omitempty"`
			} `json:"message"`
			Delta struct {
				Content   interface{}       `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
				Refusal   string `json:"refusal,omitempty"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false // can't parse, assume not empty
	}
	if len(resp.Choices) == 0 {
		return true
	}

	for _, c := range resp.Choices {
		// Check message fields
		if c.Message.Content != nil {
			if s, ok := c.Message.Content.(string); ok && s != "" {
				return false
			}
		}
		if len(c.Message.ToolCalls) > 0 {
			return false
		}
		if c.Message.Refusal != "" {
			return false
		}
		// Check delta fields (for streaming chunks)
		if c.Delta.Content != nil {
			if s, ok := c.Delta.Content.(string); ok && s != "" {
				return false
			}
		}
		if len(c.Delta.ToolCalls) > 0 {
			return false
		}
		if c.Delta.Refusal != "" {
			return false
		}
	}
	return true
}

// IsAnthropicEmpty returns true if an Anthropic Messages response contains
// no content. Checks for empty content array, or blocks with no text or
// tool_use. Thinking blocks are excluded — thinking-only responses
// are treated as empty.
func IsAnthropicEmpty(body []byte) bool {
	var resp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
			Input    json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false // can't parse, assume not empty
	}
	if len(resp.Content) == 0 {
		return true
	}
	for _, block := range resp.Content {
		if block.Text != "" {
			return false
		}
		if block.Type == "tool_use" {
			return false // tool_use block is content
		}
	}
	return true
}

// IsOpenAIResponsesEmpty returns true if an OpenAI Responses API response
// contains no content. Checks for empty output array, or message items
// with no text content parts, or function_call items.
// Reasoning output items are excluded — reasoning-only responses
// are treated as empty.
func IsOpenAIResponsesEmpty(body []byte) bool {
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false // can't parse, assume not empty
	}
	if len(resp.Output) == 0 {
		return true
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					return false
				}
			}
		case "function_call":
			return false // function_call is content
		default:
			// reasoning, web_search_call, etc. are NOT counted as content
			// thinking-only responses should be treated as empty
		}
	}
	return true
}

// IsGeminiEmpty returns true if a Gemini generateContent response contains
// no content. Checks for empty candidates array, or candidates with no
// text parts or function calls. Thinking parts (thought: true) are excluded
// — thinking-only responses are treated as empty.
func IsGeminiEmpty(body []byte) bool {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string          `json:"text,omitempty"`
					Thought      bool            `json:"thought,omitempty"`
					FunctionCall json.RawMessage `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false // can't parse, assume not empty
	}
	if len(resp.Candidates) == 0 {
		return true
	}
	for _, candidate := range resp.Candidates {
		if len(candidate.Content.Parts) == 0 {
			continue
		}
		for _, part := range candidate.Content.Parts {
			// Skip thinking parts
			if part.Thought {
				continue
			}
			if part.Text != "" {
				return false
			}
			if part.FunctionCall != nil {
				return false
			}
		}
	}
	return true
}

// ---- Empty response detection (streaming) ----
//
// IsStreamContentLine checks if a single SSE data line indicates content
// production for the given API format. Thinking/reasoning events are
// excluded — they do not count as real content.

// IsStreamContentLine returns true if the given SSE data line indicates
// a content-producing event for the specified API format. Thinking/
// reasoning events are excluded — they do not count as real content.
func IsStreamContentLine(line string, format string) bool {
	var data string
	if strings.HasPrefix(line, "data: ") {
		data = strings.TrimPrefix(line, "data: ")
	} else {
		data = line
	}

	switch format {
	case "anthropic":
		// Anthropic content events: content_block_delta with text_delta,
		// or input_json_delta for tool calls.
		// thinking_delta is excluded — thinking-only streams should be retried.
		return strings.Contains(data, "text_delta") ||
			strings.Contains(data, "input_json_delta") ||
			strings.Contains(data, "tool_use")

	case "openai":
		// OpenAI streaming chunks have a "delta" object with "content" field.
		// Check for non-null, non-empty content.
		if strings.Contains(data, `"content"`) {
			// Exclude null and empty string content
			if strings.Contains(data, `"content":null`) {
				return false
			}
			if strings.Contains(data, `"content":""`) {
				return false
			}
			return true
		}
		// Also check for tool_calls and refusal.
		// reasoning_content is excluded — thinking-only streams should be retried.
		return strings.Contains(data, `"tool_calls"`) ||
			strings.Contains(data, `"refusal"`)

	case "openai_responses":
		// OpenAI Responses streaming events are named.
		// Content events: response.output_text.delta,
		// response.function_call_arguments.delta
		// response.reasoning_summary_text.delta is excluded — thinking-only streams should be retried.
		return strings.HasPrefix(line, "event: response.output_text.delta") ||
			strings.HasPrefix(line, "event: response.function_call_arguments.delta") ||
			strings.Contains(data, `"text"`)

	case "gemini":
		// Gemini streaming chunks have candidates with parts.
		// Check for thinking parts ("thought":true with "text") and skip them.
		if strings.Contains(data, `"thought":true`) && strings.Contains(data, `"text"`) {
			// This chunk contains a thinking part — not real content
			return strings.Contains(data, `"functionCall"`)
		}
		// Regular text parts or function calls are content.
		return strings.Contains(data, `"text"`) ||
			strings.Contains(data, `"functionCall"`)

	default:
		return false
	}
}

// ---- Stream format detection ----

// DetectStreamFormat identifies the API format of an SSE chunk by
// inspecting its event type (for named-event formats like Anthropic and
// OpenAI Responses) and its data payload (for unnamed formats like
// OpenAI Chat Completions and Gemini). Returns "" when the format
// cannot be determined from this chunk.
//
// This is used by the retry-on-empty feature to detect the actual
// format of the downstream's SSE response on the fly, since the input
// request format may differ from the downstream's response format when
// auto-translation is in effect (e.g. client sends OpenAI but downstream
// speaks Anthropic).
func DetectStreamFormat(chunk SSEChunk) string {
	switch chunk.EventType {
	case "message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop", "ping":
		return "anthropic"
	case "response.created", "response.in_progress", "response.completed",
		"response.output_item.added", "response.output_item.done",
		"response.reasoning", "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done":
		return "openai_responses"
	}

	if len(chunk.Data) == 0 {
		return ""
	}
	// OpenAI SSE has no named events — payload carries a `choices` array.
	if strings.Contains(string(chunk.Data), `"choices"`) {
		return "openai"
	}
	// Gemini SSE chunks carry a `candidates` array.
	if strings.Contains(string(chunk.Data), `"candidates"`) {
		return "gemini"
	}
	return ""
}
