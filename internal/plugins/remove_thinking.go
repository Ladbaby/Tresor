package plugins

import (
	"encoding/json"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// RemoveThinking strips "thinking" / "reasoning" content from downstream
// model responses across all 4 supported API formats:
//
//   - OpenAI Chat Completions (`choices[].message.reasoning_content`)
//   - Anthropic Messages (`content[]` blocks with `type:"thinking"`)
//   - OpenAI Responses API (`output[]` items with `type:"reasoning"`)
//   - Google Gemini (`candidates[].content.parts[]` with `thought:true`)
//
// The plugin is response-only — it never modifies outgoing requests. Both
// non-streaming and streaming (SSE) responses are handled. For Anthropic
// streaming the entire thinking content block is dropped, including any
// `signature_delta` events emitted as part of the block, so the Anthropic
// SDK does not see an orphaned signature. `usage.thinking_tokens` is
// stripped from Anthropic `message_delta` events to fully hide any trace
// of thinking in token statistics.
//
// Per-request state isolation is guaranteed by the engine, which allocates
// a fresh plugin instance per pipeline (engine.go buildPipeline).
type RemoveThinking struct {
	// detectedFormat caches the format detected from the first chunk so
	// later chunks don't re-detect. Reset on stream end.
	detectedFormat string

	// anthropicInsideThinking is true while a `type:"thinking"` content
	// block is in progress. Events received while true are dropped.
	anthropicInsideThinking bool
	// anthropicThinkingIndex is the content_block index of the in-flight
	// thinking block. Used to match the closing content_block_stop and
	// avoid dropping stop events that belong to sibling blocks.
	anthropicThinkingIndex int

	// responsesReasoningOutputIdx tracks output_index values for
	// reasoning items that were added during the stream, so the matching
	// response.output_item.done event can be dropped too.
	responsesReasoningOutputIdx map[int]struct{}
}

// PluginName returns the stable type name for deduplication.
func (t *RemoveThinking) PluginName() string { return "RemoveThinking" }

// ---- Non-streaming response handler ----

// TransformResponse strips thinking content from a non-streaming response.
// The response format is detected from the body structure itself — the
// engine does not pass the response format to transformers.
func (t *RemoveThinking) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	format := detectResponseFormat(body)
	if format == "" {
		return body, nil
	}
	t.detectedFormat = format

	switch format {
	case "openai":
		return stripOpenAIReasoning(body)
	case "anthropic":
		return stripAnthropicThinking(body)
	case "openai_responses":
		return stripResponsesReasoning(body)
	case "gemini":
		return stripGeminiThought(body)
	default:
		return body, nil
	}
}

// ---- OpenAI Chat Completions ----

// stripOpenAIReasoning deletes `reasoning_content` from each
// `choices[].message` (and `choices[].delta` for safety — some providers
// emit delta-only shapes for streaming-final messages).
func stripOpenAIReasoning(body []byte) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, nil
	}

	choices, ok := resp["choices"].([]interface{})
	if !ok {
		return body, nil
	}

	changed := false
	for _, choice := range choices {
		cm, ok := choice.(map[string]interface{})
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]interface{}); ok {
			if _, has := msg["reasoning_content"]; has {
				delete(msg, "reasoning_content")
				changed = true
			}
		}
		if delta, ok := cm["delta"].(map[string]interface{}); ok {
			if _, has := delta["reasoning_content"]; has {
				delete(delta, "reasoning_content")
				changed = true
			}
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// ---- Anthropic Messages ----

// stripAnthropicThinking filters out any `content[]` block whose
// `type == "thinking"` (the block, including any `signature` field, is
// dropped in its entirety). Also strips `usage.thinking_tokens` from the
// top-level usage block if present.
func stripAnthropicThinking(body []byte) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, nil
	}

	changed := false

	if content, ok := resp["content"].([]interface{}); ok {
		filtered := make([]interface{}, 0, len(content))
		for _, block := range content {
			bm, ok := block.(map[string]interface{})
			if !ok {
				filtered = append(filtered, block)
				continue
			}
			if bm["type"] == "thinking" {
				changed = true
				continue
			}
			filtered = append(filtered, block)
		}
		if changed {
			resp["content"] = filtered
		}
	}

	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		if _, has := usage["thinking_tokens"]; has {
			delete(usage, "thinking_tokens")
			changed = true
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// ---- OpenAI Responses API ----

// stripResponsesReasoning filters out any `output[]` item whose
// `type == "reasoning"`. Other items (messages, function_call, etc.)
// are preserved in their original order.
func stripResponsesReasoning(body []byte) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, nil
	}

	output, ok := resp["output"].([]interface{})
	if !ok {
		return body, nil
	}

	filtered := make([]interface{}, 0, len(output))
	changed := false
	for _, item := range output {
		im, ok := item.(map[string]interface{})
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if im["type"] == "reasoning" {
			changed = true
			continue
		}
		filtered = append(filtered, item)
	}

	if !changed {
		return body, nil
	}
	resp["output"] = filtered
	out, err := json.Marshal(resp)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// ---- Google Gemini ----

// stripGeminiThought filters out any `candidates[].content.parts[]`
// whose `thought == true`. Text-only parts are preserved.
func stripGeminiThought(body []byte) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, nil
	}

	candidates, ok := resp["candidates"].([]interface{})
	if !ok {
		return body, nil
	}

	changed := false
	for _, cand := range candidates {
		cm, ok := cand.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := cm["content"].(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]interface{})
		if !ok {
			continue
		}
		filtered := make([]interface{}, 0, len(parts))
		localChanged := false
		for _, part := range parts {
			pm, ok := part.(map[string]interface{})
			if !ok {
				filtered = append(filtered, part)
				continue
			}
			if isTrue(pm["thought"]) {
				localChanged = true
				continue
			}
			filtered = append(filtered, part)
		}
		if localChanged {
			content["parts"] = filtered
			changed = true
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// ---- Streaming response handler ----

// TransformStreamChunk handles one SSE event from the downstream. The
// engine (engine.go handleStreamingResponse) skips writing the event when
// `len(chunk.Data) == 0`, so returning an empty chunk drops the event.
func (t *RemoveThinking) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	// Lazy format detection on the first event. For named-event formats
	// (Anthropic, Responses) the event type alone is enough. For OpenAI
	// and Gemini the data payload carries the discriminator.
	if t.detectedFormat == "" {
		t.detectedFormat = engine.DetectStreamFormat(chunk)
	}

	// Per-format stream termination → reset state.
	if t.detectedFormat == "anthropic" && chunk.EventType == "message_stop" {
		t.resetState()
		return chunk, nil
	}
	if t.detectedFormat == "openai" && strings.TrimSpace(string(chunk.Data)) == "[DONE]" {
		t.resetState()
		return chunk, nil
	}
	if t.detectedFormat == "openai_responses" && chunk.EventType == "response.completed" {
		t.resetState()
		// Fall through to the per-format handler so the completed event's
		// output[] array still has reasoning items stripped before being
		// written to the client.
		out, err := t.transformResponsesStreamChunk(chunk)
		return out, err
	}

	switch t.detectedFormat {
	case "openai":
		return t.transformOpenAIStreamChunk(chunk)
	case "anthropic":
		return t.transformAnthropicStreamChunk(chunk)
	case "openai_responses":
		return t.transformResponsesStreamChunk(chunk)
	case "gemini":
		return t.transformGeminiStreamChunk(chunk)
	default:
		return chunk, nil
	}
}

// ---- OpenAI Chat Completions streaming ----

// transformOpenAIStreamChunk deletes `delta.reasoning_content` from each
// `choices[]` of an OpenAI SSE chunk. OpenAI SSE has no named events, so
// chunk.EventType is always "". `[DONE]` is handled in the dispatcher.
func (t *RemoveThinking) transformOpenAIStreamChunk(chunk engine.SSEChunk) (engine.SSEChunk, error) {
	if len(chunk.Data) == 0 {
		return chunk, nil
	}

	var data map[string]interface{}
	if err := json.Unmarshal(chunk.Data, &data); err != nil {
		return chunk, nil
	}

	choices, ok := data["choices"].([]interface{})
	if !ok {
		return chunk, nil
	}

	changed := false
	for _, choice := range choices {
		cm, ok := choice.(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := cm["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := delta["reasoning_content"]; has {
			delete(delta, "reasoning_content")
			changed = true
		}
	}

	if !changed {
		return chunk, nil
	}
	out, err := json.Marshal(data)
	if err != nil {
		return chunk, nil
	}
	return engine.SSEChunk{EventType: "", Data: out}, nil
}

// ---- Anthropic Messages streaming ----

// transformAnthropicStreamChunk drops every event that belongs to a
// `type:"thinking"` content block: the opening content_block_start, all
// content_block_delta (including thinking_delta AND signature_delta),
// and the matching content_block_stop. Also strips usage.thinking_tokens
// from message_delta. message_start / message_delta / message_stop pass
// through unchanged.
func (t *RemoveThinking) transformAnthropicStreamChunk(chunk engine.SSEChunk) (engine.SSEChunk, error) {
	data := string(chunk.Data)

	switch chunk.EventType {
	case "content_block_start":
		// Enter a thinking block: capture the index and drop the event.
		if strings.Contains(data, `"type":"thinking"`) {
			t.anthropicInsideThinking = true
			t.anthropicThinkingIndex = extractBlockIndex(data)
			return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
		}
		return chunk, nil

	case "content_block_delta":
		// Drop ONLY deltas whose index matches the thinking block.
		// Deltas for sibling blocks (different index) MUST pass through —
		// otherwise a model that emits thinking first then text (e.g. the
		// real-world stream in completely_stripped_anthropic_response.txt
		// where thinking is at index 0 and text is at index 1) will lose
		// every text_delta and the client sees an empty response.
		// This also covers signature_delta (which always carries the
		// thinking block's index) so we don't emit an orphan signature.
		if t.anthropicInsideThinking {
			deltaIndex := extractBlockIndex(data)
			if deltaIndex == t.anthropicThinkingIndex {
				return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
			}
		}
		return chunk, nil

	case "content_block_stop":
		// Drop the stop event ONLY if it closes the thinking block we
		// opened. A stop for a sibling block (e.g. an inner text block
		// that was opened before thinking) must pass through so the
		// output stays in order.
		if t.anthropicInsideThinking {
			stopIndex := extractBlockIndex(data)
			if stopIndex == t.anthropicThinkingIndex {
				t.anthropicInsideThinking = false
				t.anthropicThinkingIndex = -1
				return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
			}
		}
		return chunk, nil

	case "message_delta":
		// Strip thinking_tokens from the usage block, then pass through.
		out, err := stripAnthropicThinkingTokensInMessageDelta(chunk.Data)
		if err != nil {
			return chunk, nil
		}
		if out != nil {
			return engine.SSEChunk{EventType: chunk.EventType, Data: out}, nil
		}
		return chunk, nil

	case "message_start", "message_stop", "ping":
		return chunk, nil

	default:
		return chunk, nil
	}
}

// stripAnthropicThinkingTokensInMessageDelta removes `thinking_tokens`
// from the `usage` block of an Anthropic `message_delta` event. Returns
// (nil, nil) when nothing changed so the caller can pass the chunk
// through unchanged.
func stripAnthropicThinkingTokensInMessageDelta(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil
	}
	usage, ok := payload["usage"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	if _, has := usage["thinking_tokens"]; !has {
		return nil, nil
	}
	delete(usage, "thinking_tokens")

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, nil
	}
	return out, nil
}

// ---- OpenAI Responses API streaming ----

// transformResponsesStreamChunk drops every event that belongs to a
// reasoning item, and strips reasoning items from the output array of
// `response.created` / `response.in_progress` / `response.completed`
// events.
func (t *RemoveThinking) transformResponsesStreamChunk(chunk engine.SSEChunk) (engine.SSEChunk, error) {
	eventType := chunk.EventType
	data := string(chunk.Data)

	// Drop any reasoning-shaped event outright.
	switch eventType {
	case "response.reasoning",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done":
		return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
	}

	// Lazy-init the tracking map.
	if t.responsesReasoningOutputIdx == nil {
		t.responsesReasoningOutputIdx = make(map[int]struct{})
	}

	switch eventType {
	case "response.output_item.added":
		// Track reasoning items so we can drop their matching done event
		// later. The event itself is dropped from the wire.
		if strings.Contains(data, `"type":"reasoning"`) {
			idx := extractResponsesOutputIndex(data)
			if idx >= 0 {
				t.responsesReasoningOutputIdx[idx] = struct{}{}
			}
			return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
		}
		return chunk, nil

	case "response.output_item.done":
		// Drop the done event if it closes a reasoning item.
		idx := extractResponsesOutputIndex(data)
		if _, isReasoning := t.responsesReasoningOutputIdx[idx]; isReasoning {
			return engine.SSEChunk{EventType: "", Data: []byte{}}, nil
		}
		return chunk, nil

	case "response.created", "response.in_progress", "response.completed":
		// Strip reasoning items from the output array in the envelope.
		return stripResponsesOutputInEvent(chunk)

	default:
		return chunk, nil
	}
}

// stripResponsesOutputInEvent removes reasoning items from the
// `output[]` of a Responses API envelope event (created / in_progress /
// completed). The completed event nests its output under
// `response.output`; created and in_progress do not carry an output
// list. If no output array is present or no reasoning items are found,
// the chunk passes through unchanged.
func stripResponsesOutputInEvent(chunk engine.SSEChunk) (engine.SSEChunk, error) {
	if len(chunk.Data) == 0 {
		return chunk, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(chunk.Data, &payload); err != nil {
		return chunk, nil
	}

	// Locate the output array. For `response.completed` it lives under
	// `response.output`; if not found, try the top level as a fallback.
	var output []interface{}
	if resp, ok := payload["response"].(map[string]interface{}); ok {
		if o, ok := resp["output"].([]interface{}); ok {
			output = o
		}
	}
	if output == nil {
		if o, ok := payload["output"].([]interface{}); ok {
			output = o
		}
	}
	if output == nil {
		return chunk, nil
	}

	filtered := make([]interface{}, 0, len(output))
	changed := false
	for _, item := range output {
		im, ok := item.(map[string]interface{})
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if im["type"] == "reasoning" {
			changed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !changed {
		return chunk, nil
	}

	// Write the filtered array back to the same location we read it from.
	if resp, ok := payload["response"].(map[string]interface{}); ok {
		if _, has := resp["output"]; has {
			resp["output"] = filtered
		} else {
			payload["output"] = filtered
		}
	} else {
		payload["output"] = filtered
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return chunk, nil
	}
	return engine.SSEChunk{EventType: chunk.EventType, Data: out}, nil
}

// ---- Google Gemini streaming ----

// transformGeminiStreamChunk deletes thought:true parts from each
// `candidates[].content.parts[]` of a Gemini SSE chunk. The Gemini API
// sends chunks with no named event type, so chunk.EventType is always "".
func (t *RemoveThinking) transformGeminiStreamChunk(chunk engine.SSEChunk) (engine.SSEChunk, error) {
	if len(chunk.Data) == 0 {
		return chunk, nil
	}

	var data map[string]interface{}
	if err := json.Unmarshal(chunk.Data, &data); err != nil {
		return chunk, nil
	}

	candidates, ok := data["candidates"].([]interface{})
	if !ok {
		return chunk, nil
	}

	changed := false
	for _, cand := range candidates {
		cm, ok := cand.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := cm["content"].(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]interface{})
		if !ok {
			continue
		}
		filtered := make([]interface{}, 0, len(parts))
		localChanged := false
		for _, part := range parts {
			pm, ok := part.(map[string]interface{})
			if !ok {
				filtered = append(filtered, part)
				continue
			}
			if isTrue(pm["thought"]) {
				localChanged = true
				continue
			}
			filtered = append(filtered, part)
		}
		if localChanged {
			content["parts"] = filtered
			changed = true
		}
	}

	if !changed {
		return chunk, nil
	}
	out, err := json.Marshal(data)
	if err != nil {
		return chunk, nil
	}
	return engine.SSEChunk{EventType: "", Data: out}, nil
}

// ---- Format detection ----

// detectResponseFormat identifies the API format of a non-streaming
// response body by inspecting its top-level structure.
func detectResponseFormat(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}

	// Anthropic Messages: top-level `content` array of typed blocks,
	// OR presence of `stop_reason` (Anthropic-specific).
	if _, has := m["content"]; has {
		if content, ok := m["content"].([]interface{}); ok && len(content) > 0 {
			if block, ok := content[0].(map[string]interface{}); ok {
				if _, hasType := block["type"]; hasType {
					return "anthropic"
				}
			}
		}
		if _, has := m["stop_reason"]; has {
			return "anthropic"
		}
	}

	// OpenAI Responses API: top-level `output` array.
	if _, has := m["output"]; has {
		return "openai_responses"
	}

	// OpenAI Chat Completions: top-level `choices` array.
	if _, has := m["choices"]; has {
		return "openai"
	}

	// Google Gemini: top-level `candidates` array.
	if _, has := m["candidates"]; has {
		return "gemini"
	}

	return ""
}

// ---- Helpers ----

// resetState clears all per-stream state. Called on format-specific
// terminal events so the next stream starts clean.
func (t *RemoveThinking) resetState() {
	t.detectedFormat = ""
	t.anthropicInsideThinking = false
	t.anthropicThinkingIndex = -1
	t.responsesReasoningOutputIdx = nil
}

// isTrue reports whether v should be treated as JSON `true`. Decoded JSON
// booleans are always Go bools, but be defensive about any other types
// the upstream may emit.
func isTrue(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}

// extractResponsesOutputIndex parses `"output_index":N` from an SSE data
// payload. Returns -1 if the field is absent or unparseable. Cheap scan
// — avoid full json.Unmarshal on every SSE chunk.
func extractResponsesOutputIndex(data string) int {
	const key = `"output_index":`
	i := strings.Index(data, key)
	if i < 0 {
		return -1
	}
	rest := data[i+len(key):]
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
var _ engine.ResponseTransformer = (*RemoveThinking)(nil)
var _ engine.StreamResponseTransformer = (*RemoveThinking)(nil)
