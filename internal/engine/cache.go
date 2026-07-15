package engine

import (
	"encoding/json"
)

// UsageFromResponseBody parses a (possibly streaming, possibly
// non-streaming) downstream response body and returns a sparsely populated
// UsageBlock. The helper is tolerant of partial / fragmented SSE bodies:
// a stream whose final usage event has not yet arrived will simply return
// a UsageBlock with whichever fields were already seen (which the caller
// is expected to merge into a running accumulator).
//
// Returns (nil, false) if the body isn't a recognised usage shape or
// fails to parse. Callers that receive (nil, false) should leave their
// accumulator untouched.
func UsageFromResponseBody(body []byte) (*UsageBlock, bool) {
	if len(body) == 0 {
		return nil, false
	}
	// Top-level array → batch-style response without per-row usage.
	bodyTrim := body
	if bodyTrim[0] == '[' {
		return nil, false
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(bodyTrim, &doc); err != nil {
		return nil, false
	}
	return UsageFromDecodedUsage(doc)
}

// UsageFromDecodedUsage walks a decoded JSON value that *may* contain a
// usage block (either at the top level, in a `message` wrapper, or
// inside `usageMetadata`) and returns the corresponding UsageBlock. The
// function is intentionally permissive about which shape the upstream
// picked — Anthropic streaming wraps usage under `message`, OpenAI puts
// it at the top level, Gemini uses `usageMetadata`.
func UsageFromDecodedUsage(doc map[string]interface{}) (*UsageBlock, bool) {
	usage, ok := locateUsageBlock(doc)
	if !ok {
		return nil, false
	}
	return usageFromMap(usage), true
}

// locateUsageBlock returns the map whose keys are cache token fields, no
// matter where in the envelope the upstream put it. Returns nil/false
// when no recognised location carries a usage block.
func locateUsageBlock(doc map[string]interface{}) (map[string]interface{}, bool) {
	if u, ok := doc["usage"].(map[string]interface{}); ok && looksLikeUsage(u) {
		return u, true
	}
	if m, ok := doc["message"].(map[string]interface{}); ok {
		if u, ok := m["usage"].(map[string]interface{}); ok && looksLikeUsage(u) {
			return u, true
		}
	}
	if u, ok := doc["usageMetadata"].(map[string]interface{}); ok && looksLikeUsage(u) {
		return u, true
	}
	return nil, false
}

// looksLikeUsage returns true when m contains at least one cache-relevant
// token field. We require *some* signal — request blocks that contain
// only `output_tokens` for a non-streaming response would be noise.
func looksLikeUsage(m map[string]interface{}) bool {
	candidates := []string{
		"input_tokens", "output_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"prompt_tokens", "completion_tokens", "cached_tokens",
		"promptTokenCount", "candidatesTokenCount",
		"cachedContentTokenCount",
	}
	for _, k := range candidates {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// usageFromMap converts a decoded JSON usage map into a UsageBlock. All
// fields are pointers so we can preserve "absent" semantics: a stream
// that reported only one side of the usage on a particular event keeps
// the other side unset so a later merge doesn't clobber a non-zero
// value with a zero default.
func usageFromMap(m map[string]interface{}) *UsageBlock {
	u := &UsageBlock{}
	for k, v := range m {
		switch k {
		case "input_tokens":
			u.InputTokens = ptrInt(asInt64(v))
		case "output_tokens":
			u.OutputTokens = ptrInt(asInt64(v))
		case "cache_creation_input_tokens":
			u.CacheCreationTokens = ptrInt(asInt64(v))
		case "cache_read_input_tokens":
			u.CacheReadTokens = ptrInt(asInt64(v))
		case "cached_tokens":
			u.CachedTokens = ptrInt(asInt64(v))
		case "prompt_tokens":
			// OpenAI Chat Completions: prompt_tokens already counts
			// cached tokens, so we treat it as input_tokens for the
			// cache-rate calculation in CacheHitRate.
			u.InputTokens = ptrInt(asInt64(v))
		}
	}
	// Also accept the nested prompt_tokens_details.cached_tokens shape.
	if details, ok := m["prompt_tokens_details"].(map[string]interface{}); ok {
		if v, ok := details["cached_tokens"]; ok {
			u.CachedTokens = ptrInt(asInt64(v))
		}
	}
	// Gemini: usageMetadata field names are camelCase.
	for k, v := range m {
		switch k {
		case "promptTokenCount":
			u.InputTokens = ptrInt(asInt64(v))
		case "candidatesTokenCount":
			u.OutputTokens = ptrInt(asInt64(v))
		case "cachedContentTokenCount":
			u.CacheReadTokens = ptrInt(asInt64(v))
		}
	}
	return u
}

// ptrInt is a tiny helper to take the address of a literal int64
// without intermediate variables.
func ptrInt(v int64) *int64 { return &v }

// asInt64 coerces a decoded JSON numeric value to int64. encoding/json
// always decodes numbers to float64; defensive coercion lets us accept
// any numeric kind without panicking on weird inputs.
func asInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
