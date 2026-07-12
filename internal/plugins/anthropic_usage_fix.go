package plugins

import (
	"encoding/json"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// Anthropic usage field names. Centralised so the helpers below and the
// message_start / message_delta paths can't drift out of sync.
const (
	usageFieldInput    = "input_tokens"
	usageFieldOutput   = "output_tokens"
	usageFieldCacheCre = "cache_creation_input_tokens"
	usageFieldCacheRd  = "cache_read_input_tokens"
)

// FixAnthropicUsage normalizes the `usage` block on Anthropic Messages
// responses. Some providers (e.g. MiniMax-M3) emit responses whose usage
// block is incomplete: message_start.usage is missing the cache token
// fields entirely, and message_delta has no usage block at all. Downstream
// SDKs such as the Anthropic TypeScript SDK and pi-agent read these fields
// unconditionally and throw on the missing properties.
//
// The plugin is a no-op on responses that already conform, so it is safe
// to leave attached even on providers that do not need it.
//
// Streaming state is per-pipeline-instance: buildPipeline allocates a
// fresh plugin per request (engine.go:558), so concurrent streams do not
// share this state.
type FixAnthropicUsage struct {
	startInputTokens  int
	startOutputTokens int
	startCacheCre     int
	startCacheRd      int
}

// PluginName returns the stable type name for deduplication.
func (t *FixAnthropicUsage) PluginName() string { return "FixAnthropicUsage" }

// TransformResponse patches the usage block on a non-streaming Anthropic
// Messages response. Missing canonical fields are added as 0; if usage is
// absent or not an object the body is passed through unchanged.
func (t *FixAnthropicUsage) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		return body, nil
	}
	usage, ok := response["usage"].(map[string]interface{})
	if !ok {
		return body, nil
	}
	if !normalizeUsageMap(usage) {
		return body, nil
	}
	newBody, err := json.Marshal(response)
	if err != nil {
		return body, nil
	}
	return newBody, nil
}

// TransformStreamChunk rewrites message_start and message_delta events so
// their usage blocks match the canonical Anthropic schema.
func (t *FixAnthropicUsage) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	switch chunk.EventType {
	case "message_start":
		newData, err := t.patchMessageStartUsage(chunk.Data)
		if err != nil {
			return chunk, nil
		}
		return engine.SSEChunk{EventType: chunk.EventType, Data: newData}, nil

	case "message_delta":
		newData, err := synthesizeMessageDeltaUsage(chunk.Data, t.startInputTokens, t.startOutputTokens, t.startCacheCre, t.startCacheRd)
		if err != nil {
			return chunk, nil
		}
		return engine.SSEChunk{EventType: chunk.EventType, Data: newData}, nil

	case "message_stop":
		t.startInputTokens = 0
		t.startOutputTokens = 0
		t.startCacheCre = 0
		t.startCacheRd = 0
		return chunk, nil

	default:
		return chunk, nil
	}
}

// patchMessageStartUsage rewrites a message_start payload so its inner
// message.usage contains all four canonical fields. Records the observed
// counts on the receiver so a later message_delta can synthesize a
// matching usage block. Returns the original bytes if no rewrite is needed.
func (t *FixAnthropicUsage) patchMessageStartUsage(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	var payload struct {
		Message map[string]interface{} `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, err
	}
	if payload.Message == nil {
		return data, nil
	}

	usageRaw, exists := payload.Message["usage"]
	usage, ok := usageRaw.(map[string]interface{})
	if exists && !ok {
		// usage is present but not an object — don't try to rewrite it.
		return data, nil
	}
	if !exists {
		usage = map[string]interface{}{}
		payload.Message["usage"] = usage
	}

	// Record the observed counts BEFORE the early-out so message_delta
	// synthesis works even when the upstream usage is already complete.
	t.startInputTokens = int(asFloat(usage[usageFieldInput]))
	t.startOutputTokens = int(asFloat(usage[usageFieldOutput]))
	t.startCacheCre = int(asFloat(usage[usageFieldCacheCre]))
	t.startCacheRd = int(asFloat(usage[usageFieldCacheRd]))

	if !normalizeUsageMap(usage) {
		return data, nil
	}

	newBody, err := json.Marshal(payload)
	if err != nil {
		return data, err
	}
	return newBody, nil
}

// synthesizeMessageDeltaUsage injects a `usage` field into a message_delta
// payload when the upstream omitted it. If a usage block is already present
// the chunk passes through unchanged.
func synthesizeMessageDeltaUsage(data []byte, inTok, outTok, cacheCre, cacheRd int) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	// Cheap early-out: most conforming providers include `"usage"` in the
	// payload. Avoid the full Unmarshal when there's nothing to do.
	if strings.Contains(string(data), `"usage"`) {
		return data, nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, nil
	}
	if _, exists := payload["usage"]; exists {
		return data, nil
	}
	payload["usage"] = map[string]interface{}{
		usageFieldInput:    inTok,
		usageFieldOutput:   outTok,
		usageFieldCacheCre: cacheCre,
		usageFieldCacheRd:  cacheRd,
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		return data, err
	}
	return newBody, nil
}

// normalizeUsageMap adds the four canonical Anthropic usage fields to m
// if they are not already present. Returns true if any field was added.
func normalizeUsageMap(m map[string]interface{}) bool {
	changed := false
	if _, ok := m[usageFieldInput]; !ok {
		m[usageFieldInput] = 0
		changed = true
	}
	if _, ok := m[usageFieldOutput]; !ok {
		m[usageFieldOutput] = 0
		changed = true
	}
	if _, ok := m[usageFieldCacheCre]; !ok {
		m[usageFieldCacheCre] = 0
		changed = true
	}
	if _, ok := m[usageFieldCacheRd]; !ok {
		m[usageFieldCacheRd] = 0
		changed = true
	}
	return changed
}

// asFloat coerces a decoded JSON numeric value to float64. JSON numbers
// always decode to float64 in Go's encoding/json, but defensively handle
// other numeric kinds for plugins that synthesised the map directly.
func asFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

// Ensure interface compliance.
var _ engine.ResponseTransformer = (*FixAnthropicUsage)(nil)
var _ engine.StreamResponseTransformer = (*FixAnthropicUsage)(nil)