package engine

import (
	"encoding/json"
	"math"
)

// UsageBlock is the cache-hit-rate "ground truth" stored on every log
// entry whose downstream sent a recognisable usage block. It mirrors the
// shape produced by the web UI's `normalizeUsage` helper (see
// internal/api/web/sse-reassembler.js), so the Logs tab and the
// Inspect view compute the cache-hit rate from the same fields. This
// avoids the two surfaces drifting apart: if a downstream's SSE changes
// shape, both surfaces update in step.
//
// All fields are pointers so the block can be sparsely populated across
// multiple SSE events. The Anthropic protocol spreads the final usage
// across `message_start` (input_tokens + cache_read) and `message_delta`
// (cache_creation + output_tokens); we accumulate whichever fields each
// event supplied and merge them into one block.
type UsageBlock struct {
	// InputTokens is the non-cached portion of the prompt (Anthropic),
	// or the entire prompt token count (OpenAI / Gemini).
	InputTokens *int64 `json:"input_tokens,omitempty"`

	// OutputTokens is the number of tokens generated for the response.
	OutputTokens *int64 `json:"output_tokens,omitempty"`

	// CacheCreationTokens is how many tokens the downstream wrote to
	// its prompt cache on this request (Anthropic).
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens,omitempty"`

	// CacheReadTokens is how many tokens were served from the
	// downstream's prompt cache (Anthropic).
	CacheReadTokens *int64 `json:"cache_read_input_tokens,omitempty"`

	// CachedTokens is the equivalent of CacheReadTokens for the
	// OpenAI Chat Completions / Responses API. It lives in a different
	// field name because the formats have different conventions; the
	// web UI's normalizeUsage maps this back to cache_read_input_tokens
	// for display, so both shapes collapse to the same metric.
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}

// Merge folds o into u, keeping u's non-nil values. OpenAI's
// `cached_tokens` and Anthropic's `cache_read_input_tokens` are
// semantically equivalent and we treat both as a "cache hit" count.
//
// Note: this only assigns when o's field is non-nil — it does not blank
// out u's previously-set value if o's corresponding field happens to be
// nil. That matches the SSE semantics where subsequent events (e.g. the
// message_delta's output-only update) must not clobber fields that were
// only reported earlier.
func (u *UsageBlock) Merge(o *UsageBlock) {
	if u == nil || o == nil {
		return
	}
	if o.InputTokens != nil {
		u.InputTokens = o.InputTokens
	}
	if o.OutputTokens != nil {
		u.OutputTokens = o.OutputTokens
	}
	if o.CacheCreationTokens != nil {
		u.CacheCreationTokens = o.CacheCreationTokens
	}
	if o.CacheReadTokens != nil {
		u.CacheReadTokens = o.CacheReadTokens
	}
	if o.CachedTokens != nil {
		u.CachedTokens = o.CachedTokens
	}
}

// CachedTotal returns the total number of prompt-cache tokens served
// from cache on this request, unifying Anthropic and OpenAI shapes.
func (u *UsageBlock) CachedTotal() int64 {
	var n int64
	if u.CacheReadTokens != nil {
		n += *u.CacheReadTokens
	}
	if u.CachedTokens != nil {
		n += *u.CachedTokens
	}
	return n
}

// CacheHitRate computes the cache-hit rate as a fraction in [0.0, 1.0].
// The denominator is the total prompt size — cached + fresh + newly
// created — because every token had to be processed one way or another,
// and the cached portion reflects hits.
//
// Returns (nil, false) when no cache-relevant field is present (the
// caller should treat that as "no information"). If cache info is
// present (cache_read_input_tokens or cached_tokens), the function
// returns a pointer rounded to 0.01% precision (1e-4); even 0% is a
// real answer when the upstream reported a zero cache hit.
func (u *UsageBlock) CacheHitRate() (*float64, bool) {
	if u == nil {
		return nil, false
	}
	hasAnyCacheField := u.CacheReadTokens != nil || u.CachedTokens != nil
	if !hasAnyCacheField {
		// No cache information whatsoever. The caller should treat
		// this as "absent" rather than "0%".
		return nil, false
	}
	cached := u.CachedTotal()
	if cached < 0 {
		return nil, false
	}
	var input, creation int64
	if u.InputTokens != nil {
		input = *u.InputTokens
	}
	if u.CacheCreationTokens != nil {
		creation = *u.CacheCreationTokens
	}
	if u.CachedTokens != nil && u.InputTokens != nil {
		// OpenAI-style: prompt_tokens already includes the cached
		// portion. Denominator is just the prompt size, not a sum.
		denom := float64(input)
		if denom <= 0 {
			return nil, false
		}
		r := math.Round((float64(*u.CachedTokens)/denom)*10000) / 10000
		return &r, true
	}
	denom := float64(cached) + float64(input) + float64(creation)
	if denom <= 0 {
		return nil, false
	}
	r := math.Round((float64(cached)/denom)*10000) / 10000
	return &r, true
}

// MarshalJSON ensures a nil UsageBlock serialises as `null` rather than
// as the empty object `{}`. Together with the `omitempty` tag on the
// field that holds it, the absence of `usage` on the wire means
// "inspector off", while `null` means "inspector on, but no usage data
// found" (the UI renders the latter as "cache N/A").
func (u *UsageBlock) MarshalJSON() ([]byte, error) {
	if u == nil {
		return []byte("null"), nil
	}
	type alias UsageBlock
	return json.Marshal((*alias)(u))
}
