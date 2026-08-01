package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"strconv"
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

// Drain flushes the buffered bytes to the underlying writer WITHOUT
// switching to pass-through mode. Subsequent writes continue to be
// buffered. This is used for incremental flushing during a stream
// (drain each event as it completes) without losing the retry
// capability — only Flush() permanently commits to no-retry.
func (bw *bufferedWriter) Drain() {
	if bw.flushed || bw.buffer.Len() == 0 {
		return
	}
	bw.writer.Write(bw.buffer.Bytes())
	bw.buffer.Reset()
	if f, ok := bw.writer.(http.Flusher); ok {
		f.Flush()
	}
}

// Reset discards any buffered bytes without writing them to the underlying
// writer. Used by the streaming retry handler when an event turned out to
// be reasoning-only / empty and should be dropped (so the upstream can be
// retried) rather than delivered to the client.
func (bw *bufferedWriter) Reset() {
	if bw.flushed {
		return
	}
	bw.buffer.Reset()
}

// IsFlushed reports whether Flush() has been called. Used to detect empty
// streams: if the writer was never flushed, no content was produced.
func (bw *bufferedWriter) IsFlushed() bool {
	return bw.flushed
}

// ---- headerDelayWriter ----
//
// Wraps an http.ResponseWriter to delay WriteHeader until Flush() is called,
// while passing Write() through immediately. This lets the streaming
// retry handler:
//   - Send bytes progressively to the client (streaming works as expected)
//   - Defer the HTTP status commitment until content is confirmed (so a
//     502 error can still be written if all retries are exhausted and no
//     content was produced)
//
// This is the new preferred wrapper for streaming responses; the older
// bufferedWriter (which buffers both Write and WriteHeader) is kept for
// non-streaming use cases.

type headerDelayWriter struct {
	writer  http.ResponseWriter
	status  int
	flushed bool
}

func newHeaderDelayWriter(w http.ResponseWriter) *headerDelayWriter {
	return &headerDelayWriter{
		writer:  w,
		status:  http.StatusOK,
		flushed: false,
	}
}

func (hw *headerDelayWriter) Header() http.Header {
	return hw.writer.Header()
}

func (hw *headerDelayWriter) Write(p []byte) (int, error) {
	if !hw.flushed {
		hw.writer.WriteHeader(hw.status)
		hw.flushed = true
	}
	return hw.writer.Write(p)
}

func (hw *headerDelayWriter) WriteHeader(status int) {
	if hw.flushed {
		hw.writer.WriteHeader(status)
	} else {
		hw.status = status
	}
}

// Flush commits the deferred status header to the underlying writer and then
// flushes it, so SSE bytes reach the client progressively.
//
// The header is only committed once; subsequent calls just flush. Note that a
// Flush before any Write does commit the status — callers that need to keep the
// status open (so a non-200 error can still be sent) must not call Flush until
// they have decided to stream.
func (hw *headerDelayWriter) Flush() {
	if !hw.flushed {
		hw.flushed = true
		hw.writer.WriteHeader(hw.status)
	}
	if f, ok := hw.writer.(http.Flusher); ok {
		f.Flush()
	}
}

// IsFlushed reports whether the status header has been committed.
func (hw *headerDelayWriter) IsFlushed() bool {
	return hw.flushed
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
		// Check for non-null, non-empty, non-empty-array content.
		if strings.Contains(data, `"content"`) {
			// Exclude null, empty string, and empty-array content
			if strings.Contains(data, `"content":null`) ||
				strings.Contains(data, `"content":""`) ||
				strings.Contains(data, `"content":[]`) {
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

// ---- In-band stream error detection ----

// StreamError describes a fatal error a downstream delivered in-band on an
// SSE stream while the HTTP status was 200.
type StreamError struct {
	Status  int    // HTTP status to surface to the client
	Message string // provider-supplied message
}

// anthropicErrorStatus maps Anthropic's error `type` values to HTTP statuses,
// used when the payload carries no numeric code.
var anthropicErrorStatus = map[string]int{
	"overloaded_error":      529,
	"rate_limit_error":      http.StatusTooManyRequests,
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"api_error":             http.StatusInternalServerError,
}

// streamErrorPayload is a permissive union of the error shapes providers emit.
// Numeric codes arrive as either a JSON number or a string, so Code fields are
// json.RawMessage and decoded by errorStatusFromRaw.
type streamErrorPayload struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Code    json.RawMessage `json:"code"`
	Status  json.RawMessage `json:"status"`
	Error   *struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// ParseStreamError reports whether an SSE event is a fatal in-band error and,
// if so, the HTTP status and message to surface.
//
// Detection is deliberately conservative: it requires either an `error` event
// name or a structural error marker in the parsed JSON. Substring matching is
// avoided so ordinary content mentioning the word "error" is never mistaken
// for a failure.
func ParseStreamError(eventType string, data []byte) (*StreamError, bool) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[DONE]" {
		return nil, false
	}

	var payload streamErrorPayload
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, false
	}

	isError := eventType == "error" ||
		eventType == "response.failed" ||
		payload.Type == "error" ||
		payload.Error != nil
	if !isError {
		return nil, false
	}

	status := 0
	errType := payload.Type
	message := payload.Message
	if payload.Error != nil {
		if payload.Error.Type != "" {
			errType = payload.Error.Type
		}
		if payload.Error.Message != "" {
			message = payload.Error.Message
		}
		status = errorStatusFromRaw(payload.Error.Code)
	}
	if status == 0 {
		status = errorStatusFromRaw(payload.Code)
	}
	if status == 0 {
		status = errorStatusFromRaw(payload.Status)
	}
	if status == 0 {
		status = anthropicErrorStatus[errType]
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	if message == "" {
		message = "upstream returned an error event"
	}

	return &StreamError{Status: status, Message: message}, true
}

// errorStatusFromRaw extracts an HTTP status from a JSON value that may be a
// number or a numeric string. Returns 0 when absent or outside 400-599.
func errorStatusFromRaw(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var code int
	if err := json.Unmarshal(raw, &code); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0
		}
		code = parsed
	}
	if code < 400 || code > 599 {
		return 0
	}
	return code
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

// isTerminalEvent returns true if the given accumulated SSE event data
// represents a stream-completion marker for the given API format. These
// events are the last ones a downstream emits, so they signal that no
// more content is coming. For retry-on-empty to work transparently, the
// gateway holds these events until end-of-stream so it can suppress them
// when the upstream needs to be retried (the client should not see
// [DONE] for an empty response).
//
// Reasoning/thinking-only streams also produce a terminal event without
// any prior real content; in that case the terminal is suppressed and
// the upstream is retried. Reasoning tokens themselves remain visible to
// the client — only the terminal marker is held.
func isTerminalEvent(data string, format string) bool {
	data = strings.TrimSpace(data)
	if data == "" {
		return false
	}
	switch format {
	case "openai":
		// OpenAI uses a sentinel [DONE] payload.
		return data == "[DONE]"
	case "anthropic":
		// Anthropic emits message_stop as a named event; the engine's
		// SSE scanner strips the "event:" prefix and accumulates the
		// JSON payload in sseEvent. Check the JSON type field.
		return strings.Contains(data, `"type":"message_stop"`) ||
			strings.Contains(data, `"type":"message_delta"`) // message_delta signals end-of-message
	case "openai_responses":
		// OpenAI Responses API uses named events ending the stream.
		// The accumulated data is the JSON payload; check the type.
		return strings.Contains(data, `"type":"response.completed"`) ||
			strings.Contains(data, `"type":"response.incomplete"`)
	case "gemini":
		// Gemini uses finishReason in candidates — terminal events
		// carry a non-empty finishReason field.
		return strings.Contains(data, `"finishReason":"MAX_TOKENS"`) ||
			strings.Contains(data, `"finishReason":"SAFETY"`) ||
			strings.Contains(data, `"finishReason":"RECITATION"`) ||
			strings.Contains(data, `"finishReason":"OTHER"`) ||
			strings.Contains(data, `"finishReason":"STOP"`)
	default:
		return false
	}
}

// isDataLineInTerminal reports whether the given line (a raw SSE line
// from the scanner) corresponds to a data line that has already been
// captured in the held terminal event. Used by the streaming handler to
// avoid writing the held terminal's bytes to the client twice (once from
// the scanner, once when the terminal is flushed).
func isDataLineInTerminal(terminalDataLines []string, line string) bool {
	if !strings.HasPrefix(line, "data: ") {
		return false
	}
	payload := strings.TrimPrefix(line, "data: ")
	for _, held := range terminalDataLines {
		if held == payload {
			return true
		}
	}
	return false
}
