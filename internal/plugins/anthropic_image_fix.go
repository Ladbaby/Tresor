package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// FixAnthropicImages extracts image parts nested inside tool_result content
// and promotes them to top-level message content parts.
//
// Some Anthropic-compatible backends (e.g. llama.cpp) cannot handle images
// nested inside tool_result.content[]. This plugin rewrites the request so
// that images become sibling parts of the user message, which all backends
// can process correctly.
//
// On the response side, it handles two issues from CoT (chain-of-thought)
// backends that emit tool_use inside thinking blocks:
// 1. Non-streaming: extracts tool_use JSON objects embedded in the thinking
//    string and promotes them to top-level content parts.
// 2. Streaming: detects tool_use SSE events emitted inside a thinking content
//    block and re-emits them as top-level events before continuing with thinking.
//
// Reference: https://github.com/ggml-org/llama.cpp/pull/22536
type FixAnthropicImages struct {
	// Streaming state
	insideThinking         bool
	trackingToolUse        bool
	bufferedToolUse        []string // accumulated SSE lines for nested tool_use events
	bufferedToolUseIndex   int      // content_block index of the buffered tool_use
}

// PluginName returns the stable type name for deduplication.
func (t *FixAnthropicImages) PluginName() string { return "FixAnthropicImages" }

// TransformRequest rewrites tool_result image content into top-level user
// image parts before forwarding the request.
func (t *FixAnthropicImages) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("fix_anthropic_images: failed to parse request: %w", err)
	}

	rewritten, changed := rewriteToolResultImages(payload)
	if !changed {
		return req, body, nil
	}

	newBody, err := json.Marshal(rewritten)
	if err != nil {
		return nil, nil, fmt.Errorf("fix_anthropic_images: failed to marshal rewritten request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}

	return newReq, newBody, nil
}

// TransformResponse handles non-streaming responses where tool_use blocks
// may be embedded inside the thinking string.
func (t *FixAnthropicImages) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		return body, nil
	}

	content, ok := response["content"].([]interface{})
	if !ok {
		return body, nil
	}

	rewritten, changed := rewriteThinkingToolUse(content)
	if !changed {
		return body, nil
	}

	response["content"] = rewritten
	newBody, err := json.Marshal(response)
	if err != nil {
		return body, nil
	}
	return newBody, nil
}

// TransformStreamChunk handles SSE streaming responses where tool_use events
// may be emitted inside a thinking content block.
func (t *FixAnthropicImages) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	data := string(chunk.Data)

	// Termination: flush any in-flight buffered tool_use before passing through.
	// Covers synthetic [DONE] (engine.go:701) and explicit message_stop.
	if t.trackingToolUse {
		if chunk.EventType == "message_stop" || strings.TrimSpace(data) == "[DONE]" {
			sse := strings.Join(t.bufferedToolUse, "\n") + "\n"
			t.bufferedToolUse = nil
			t.trackingToolUse = false
			t.bufferedToolUseIndex = 0
			t.insideThinking = false
			return engine.SSEChunk{EventType: "", Data: []byte(sse + "\n")}, nil
		}
	}

	// Handle empty data lines (SSE event terminators) — defensive. The engine
	// normally consumes empty lines itself, but if one reaches us, flush any
	// buffered tool_use so it isn't lost.
	if chunk.EventType == "" && strings.TrimSpace(data) == "" {
		if t.trackingToolUse && len(t.bufferedToolUse) > 0 {
			sse := strings.Join(t.bufferedToolUse, "\n") + "\n"
			t.bufferedToolUse = nil
			t.trackingToolUse = false
			t.bufferedToolUseIndex = 0
			// Stay inside thinking; tool_use stop may still arrive.
			return engine.SSEChunk{
				EventType: "",
				Data:      []byte(sse + "\n"),
			}, nil
		}
		return chunk, nil
	}

	// --- Inside a thinking content block ---
	if t.insideThinking {
		// content_block_start for tool_use while inside thinking = nested tool_use
		if chunk.EventType == "content_block_start" && strings.Contains(data, `"type":"tool_use"`) {
			// If a previous nested tool_use was already being tracked, flush it
			// before starting on the new one. The previous tool_use's events
			// have been accumulating in the buffer; without this flush they
			// would be silently overwritten by the next iteration and lost.
			// The previous tool_use's content_block_stop has not yet arrived,
			// so we emit only the start + deltas — its stop will pass through
			// when it eventually arrives (the index is no longer the buffered
			// one, so the mismatch-stop branch handles it).
			var prevFlush string
			if t.trackingToolUse && len(t.bufferedToolUse) > 0 {
				prevFlush = strings.Join(t.bufferedToolUse, "\n") + "\n"
				t.bufferedToolUse = nil
				t.trackingToolUse = false
				t.bufferedToolUseIndex = 0
			}

			t.trackingToolUse = true
			t.bufferedToolUseIndex = extractBlockIndex(data)
			// Absorb this chunk — it will be re-emitted when the block completes
			t.bufferedToolUse = append(t.bufferedToolUse, "event: content_block_start", "data: "+data, "")
			if prevFlush != "" {
				// Combine the previous tool_use flush with this absorbed event
				// in one chunk so the engine writes both atomically.
				return engine.SSEChunk{
					EventType: "",
					Data:      []byte(prevFlush),
				}, nil
			}
			return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
		}

		if t.trackingToolUse {
			// content_block_stop — only flush if its index matches the buffered
			// tool_use's index. Otherwise, this stop is for a sibling block
			// (e.g. the outer thinking block, a text block, or an earlier
			// tool_use whose events were already flushed) — pass it through
			// immediately. Buffering these stops produces a malformed output
			// where stop events arrive in arbitrary order with no preceding
			// start, which the Anthropic SDK rejects with "Content block not
			// found".
			if chunk.EventType == "content_block_stop" {
				stopIndex := extractBlockIndex(data)
				if stopIndex != t.bufferedToolUseIndex {
					return chunk, nil
				}
				t.bufferedToolUse = append(t.bufferedToolUse, "event: content_block_stop", "data: "+data, "")

				// Emit all buffered tool_use SSE events now, before continuing with thinking
				sse := strings.Join(t.bufferedToolUse, "\n") + "\n"
				t.bufferedToolUse = nil
				t.trackingToolUse = false
				t.bufferedToolUseIndex = 0
				// Stay inside thinking; the outer thinking stop will set us free.
				return engine.SSEChunk{
					EventType: "",
					Data:      []byte(sse + "\n"),
				}, nil
			}

			// content_block_delta for tool_use — buffer it
			if chunk.EventType == "content_block_delta" {
				t.bufferedToolUse = append(t.bufferedToolUse, "event: content_block_delta", "data: "+data, "")
				return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
			}

			// message_delta or other events while tracking — flush buffer first,
			// then pass through normally
			if len(t.bufferedToolUse) > 0 {
				sse := strings.Join(t.bufferedToolUse, "\n") + "\n"
				t.bufferedToolUse = nil
				t.trackingToolUse = false
				t.bufferedToolUseIndex = 0
				// Emit buffered + current event
				sse += "event: " + chunk.EventType + "\ndata: " + data + "\n\n"
				return engine.SSEChunk{EventType: "", Data: []byte(sse)}, nil
			}

			// Normal thinking delta — pass through
			return chunk, nil
		}

		// Not tracking, normal thinking event — pass through
		return chunk, nil
	}

	// --- Outside thinking block ---

	// Enter thinking content block
	if chunk.EventType == "content_block_start" && strings.Contains(data, `"type":"thinking"`) {
		t.insideThinking = true
		t.trackingToolUse = false
		t.bufferedToolUse = nil
		t.bufferedToolUseIndex = 0
		return engine.SSEChunk{EventType: chunk.EventType, Data: []byte(data)}, nil
	}

	// content_block_start for non-thinking — pass through
	if chunk.EventType == "content_block_start" {
		return chunk, nil
	}

	// content_block_stop — if we were inside thinking, we're exiting
	if chunk.EventType == "content_block_stop" && t.insideThinking {
		t.insideThinking = false
		t.trackingToolUse = false
		t.bufferedToolUse = nil
		t.bufferedToolUseIndex = 0
		return chunk, nil
	}

	// Everything else — pass through unchanged
	return chunk, nil
}

// extractBlockIndex pulls the `"index":N` field out of an Anthropic SSE
// content_block event's data payload. Returns -1 if not parseable.
func extractBlockIndex(data string) int {
	// Cheap scan — avoid full json.Unmarshal on every SSE chunk.
	const key = `"index":`
	i := strings.Index(data, key)
	if i < 0 {
		return -1
	}
	rest := data[i+len(key):]
	// Skip whitespace
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return -1
	}
	n := 0
	sawDigit := false
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		sawDigit = true
	}
	if !sawDigit {
		return -1
	}
	return n
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*FixAnthropicImages)(nil)
var _ engine.ResponseTransformer = (*FixAnthropicImages)(nil)
var _ engine.StreamResponseTransformer = (*FixAnthropicImages)(nil)

// rewriteToolResultImages iterates over the messages array in an Anthropic
// request payload and extracts image parts from tool_result.content[] into
// top-level message content. Returns the (possibly modified) payload and a
// boolean indicating whether any changes were made.
func rewriteToolResultImages(payload map[string]interface{}) (map[string]interface{}, bool) {
	if payload == nil {
		return payload, false
	}

	messages, ok := payload["messages"].([]interface{})
	if !ok {
		return payload, false
	}

	rewrittenMessages := make([]interface{}, 0, len(messages))
	rewroteAny := false

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			rewrittenMessages = append(rewrittenMessages, msg)
			continue
		}

		content, ok := msgMap["content"].([]interface{})
		if !ok {
			rewrittenMessages = append(rewrittenMessages, msgMap)
			continue
		}

		newContent := make([]interface{}, 0, len(content))
		extractedImages := make([]interface{}, 0)

		for _, part := range content {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				newContent = append(newContent, part)
				continue
			}

			images, textParts := extractImagePartsFromToolResult(partMap)
			if len(images) > 0 {
				extractedImages = append(extractedImages, images...)
				rewroteAny = true
				// If the tool_result also contained text parts, preserve them
				// as a separate text block so context is not lost.
				if len(textParts) > 0 {
					newContent = append(newContent, makeTextPartFromParts(textParts))
				}
				// Drop the tool_result part — images are promoted.
				continue
			}

			newContent = append(newContent, partMap)
		}

		if len(extractedImages) > 0 {
			// If removing tool_results left the message empty, add a placeholder.
			if len(newContent) == 0 {
				newContent = append(newContent, map[string]interface{}{
					"type": "text",
					"text": "[Proxy rewrite] Converted tool_result image(s) into top-level user image part(s).",
				})
			}
			newContent = append(newContent, extractedImages...)
		}

		rewrittenMsg := cloneMap(msgMap)
		rewrittenMsg["content"] = newContent
		rewrittenMessages = append(rewrittenMessages, rewrittenMsg)
	}

	if !rewroteAny {
		return payload, false
	}

	rewritten := cloneMap(payload)
	rewritten["messages"] = rewrittenMessages
	return rewritten, true
}

// cloneMap returns a shallow copy of src. Used by rewrite paths that need to
// override one field on a content part without mutating the original map.
func cloneMap(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

// extractImagePartsFromToolResult extracts image parts from a tool_result
// content block. It returns a slice of Anthropic-style image parts ready to
// be inserted as top-level message content, plus any text parts found inside
// the tool_result (so they can be preserved separately).
func extractImagePartsFromToolResult(part map[string]interface{}) ([]interface{}, []string) {
	if part["type"] != "tool_result" {
		return nil, nil
	}

	content, ok := part["content"].([]interface{})
	if !ok || len(content) == 0 {
		return nil, nil
	}

	var imageParts []interface{}
	var textParts []string

	for _, item := range content {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			// Non-map items (strings, numbers, booleans, null) are preserved as
			// text so structured tool output (status codes, success flags) isn't
			// silently dropped when the part is promoted to top-level content.
			textParts = append(textParts, fmt.Sprintf("%v", item))
			continue
		}

		if itemMap["type"] == "image" {
			source, ok := itemMap["source"].(map[string]interface{})
			if !ok || source["type"] != "base64" {
				continue
			}

			data, _ := source["data"].(string)
			if data == "" {
				continue
			}

			imageParts = append(imageParts, map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type":       "base64",
					"media_type": source["media_type"],
					"data":       data,
				},
			})
		} else if itemMap["type"] == "text" {
			if s, ok := itemMap["text"].(string); ok {
				textParts = append(textParts, s)
			}
		}
	}

	return imageParts, textParts
}

// makeTextPartFromParts combines one or more text snippets into a single
// Anthropic text content part.
func makeTextPartFromParts(parts []string) map[string]interface{} {
	combined := ""
	for i, p := range parts {
		if i > 0 {
			combined += "\n"
		}
		combined += p
	}
	return map[string]interface{}{
		"type": "text",
		"text": combined,
	}
}

// rewriteThinkingToolUse processes a response content array, extracting
// tool_use blocks that are embedded inside thinking content (as either
// JSON objects in a content array or as text patterns in a thinking string).
// Extracted tool_use blocks are promoted to top-level content parts.
func rewriteThinkingToolUse(content []interface{}) ([]interface{}, bool) {
	rewritten := make([]interface{}, 0, len(content))
	rewroteAny := false

	for _, part := range content {
		partMap, ok := part.(map[string]interface{})
		if !ok || partMap["type"] != "thinking" {
			rewritten = append(rewritten, part)
			continue
		}

		// Dispatch on where the thinking payload lives. All three shapes use
		// the same rewrite: extract tool_uses, replace the field with the
		// remainder, then promote the tool_uses to top level.
		var toolUses, remaining []interface{}
		var field string
		var newValue interface{}

		if arr, ok := partMap["content"].([]interface{}); ok && len(arr) > 0 {
			toolUses, remaining = separateToolUsesFromContent(arr)
			field, newValue = "content", remaining
		} else if str, ok := partMap["text"].(string); ok && len(str) > 0 {
			var cleaned string
			toolUses, cleaned = extractToolUseFromThinkingText(str)
			field, newValue = "text", cleaned
		} else if arr, ok := partMap["text"].([]interface{}); ok && len(arr) > 0 {
			toolUses, remaining = separateToolUsesFromContent(arr)
			field, newValue = "text", remaining
		}

		if field == "" || len(toolUses) == 0 {
			rewritten = append(rewritten, partMap)
			continue
		}
		clean := cloneMap(partMap)
		clean[field] = newValue
		rewritten = append(rewritten, clean)
		rewritten = append(rewritten, toolUses...)
		rewroteAny = true
	}

	if !rewroteAny {
		return content, false
	}
	return rewritten, true
}

// separateToolUsesFromContent splits a content array into tool_use blocks
// (promoted to top level) and remaining content (kept in thinking).
func separateToolUsesFromContent(content []interface{}) ([]interface{}, []interface{}) {
	var toolUses []interface{}
	var remaining []interface{}

	for _, item := range content {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			remaining = append(remaining, item)
			continue
		}

		if itemMap["type"] == "tool_use" {
			toolUses = append(toolUses, itemMap)
		} else {
			remaining = append(remaining, item)
		}
	}

	return toolUses, remaining
}

// extractToolUseFromThinkingText attempts to find JSON-encoded tool_use
// blocks embedded in a thinking text string. It extracts valid JSON objects
// that look like tool_use blocks and returns them as structured content parts,
// along with the cleaned thinking text (with the JSON removed).
func extractToolUseFromThinkingText(text string) ([]interface{}, string) {
	var extracted []interface{}
	type edit struct{ start, end int; replacement string }
	var edits []edit

	indices := findJSONObjects(text)
	for _, idx := range indices {
		obj, endPos := extractJSONObjectAt(text, idx)
		if obj == nil {
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(obj, &parsed); err != nil {
			continue
		}
		if parsed["type"] != "tool_use" {
			continue
		}
		extracted = append(extracted, parsed)
		edits = append(edits, edit{start: idx, end: endPos, replacement: "[tool_use: " + parsedToString(parsed) + "]"})
	}

	if len(edits) == 0 {
		return nil, text
	}

	// Apply edits against the original text by walking it once. Edits are
	// in increasing start order (findJSONObjects scans left-to-right), so we
	// emit unchanged runs between edits without any offset arithmetic.
	var b strings.Builder
	cur := 0
	for _, e := range edits {
		b.WriteString(text[cur:e.start])
		b.WriteString(e.replacement)
		cur = e.end
	}
	b.WriteString(text[cur:])
	return extracted, b.String()
}

// findJSONObjects returns the starting positions of all JSON object literals
// in the text (positions of '{' characters that appear to start JSON objects).
func findJSONObjects(text string) []int {
	var indices []int
	for i := 0; i < len(text); i++ {
		if text[i] == '{' {
			// Quick check: is there a matching '}' ahead?
			if strings.ContainsRune(text[i:], '}') {
				// Check if this looks like JSON (has ":" or "type" nearby)
				chunk := text[i:]
				if len(chunk) > 10 && strings.Contains(chunk[:10], "\"") {
					indices = append(indices, i)
				}
			}
		}
	}
	return indices
}

// extractJSONObjectAt tries to extract a valid JSON object starting at or
// after position pos. Returns the JSON bytes and the end position, or
// (nil, 0) on failure. On a JSON validation failure at the first candidate
// position, it advances one byte and tries again — this recovers from
// instances where the brace matcher closes on an `}` that lives inside an
// unescaped string (some malformed tool_use payloads do this), at the cost
// of a bounded retry.
func extractJSONObjectAt(text string, pos int) ([]byte, int) {
	const maxRetries = 64
	for attempt := 0; attempt < maxRetries && pos < len(text); attempt++ {
		if text[pos] != '{' {
			return nil, 0
		}
		depth := 0
		inString := false
		escapeNext := false

		for i := pos; i < len(text); i++ {
			c := text[i]
			if escapeNext {
				escapeNext = false
				continue
			}
			if c == '\\' && inString {
				escapeNext = true
				continue
			}
			if c == '"' {
				inString = !inString
				continue
			}
			if inString {
				continue
			}
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 {
					obj := []byte(text[pos : i+1])
					var check map[string]interface{}
					if json.Unmarshal(obj, &check) == nil {
						return obj, i + 1
					}
					// Validation failed — this `}` was probably inside a string.
					// Advance past the bad start `{` and retry.
					pos++
					break
				}
			}
		}
		if depth != 0 {
			return nil, 0
		}
	}
	return nil, 0
}

// parsedToString creates a compact identifier for a tool_use block
// (e.g. "tool_use my_tool id=abc123") for the thinking text replacement.
func parsedToString(obj map[string]interface{}) string {
	name := ""
	if n, ok := obj["name"].(string); ok {
		name = n
	}
	id := ""
	if i, ok := obj["id"].(string); ok {
		id = i
	}
	if name != "" && id != "" {
		return name + " " + id
	}
	if name != "" {
		return name
	}
	return "tool_use"
}
