package engine

import (
	"encoding/json"
	"testing"
)

func TestUsageFromResponseBody_AnthropicMessages(t *testing.T) {
	body := `{"id":"msg_01","usage":{"input_tokens":29,"cache_read_input_tokens":9,"cache_creation_input_tokens":0,"output_tokens":170}}`
	u, ok := UsageFromResponseBody([]byte(body))
	if !ok || u == nil {
		t.Fatalf("expected usage block")
	}
	if u.InputTokens == nil || *u.InputTokens != 29 {
		t.Errorf("input_tokens: got %v, want 29", u.InputTokens)
	}
	if u.OutputTokens == nil || *u.OutputTokens != 170 {
		t.Errorf("output_tokens: got %v, want 170", u.OutputTokens)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 9 {
		t.Errorf("cache_read_input_tokens: got %v, want 9", u.CacheReadTokens)
	}
	if u.CacheCreationTokens == nil || *u.CacheCreationTokens != 0 {
		t.Errorf("cache_creation_input_tokens: got %v, want 0", u.CacheCreationTokens)
	}
}

func TestUsageFromResponseBody_OpenAIChat(t *testing.T) {
	body := `{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":91}}}`
	u, ok := UsageFromResponseBody([]byte(body))
	if !ok || u == nil {
		t.Fatalf("expected usage block")
	}
	if u.InputTokens == nil || *u.InputTokens != 100 {
		t.Errorf("input_tokens (from prompt_tokens): got %v, want 100", u.InputTokens)
	}
	if u.CachedTokens == nil || *u.CachedTokens != 91 {
		t.Errorf("cached_tokens (from prompt_tokens_details): got %v, want 91", u.CachedTokens)
	}
}

func TestUsageFromResponseBody_Gemini(t *testing.T) {
	body := `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":50,"cachedContentTokenCount":910}}`
	u, ok := UsageFromResponseBody([]byte(body))
	if !ok || u == nil {
		t.Fatalf("expected usage block")
	}
	if u.InputTokens == nil || *u.InputTokens != 1000 {
		t.Errorf("input_tokens: got %v, want 1000", u.InputTokens)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 910 {
		t.Errorf("cache_read_input_tokens (from cachedContentTokenCount): got %v, want 910", u.CacheReadTokens)
	}
}

func TestUsageFromResponseBody_NoUsageBlock(t *testing.T) {
	body := `{"id":"foo","choices":[{"message":{"content":"hi"}}]}`
	if u, ok := UsageFromResponseBody([]byte(body)); ok || u != nil {
		t.Fatalf("expected ok=false when no usage block, got %v ok=%v", u, ok)
	}
}

func TestUsageFromResponseBody_AnthropicStreamingMessageStart(t *testing.T) {
	// Real-world case: the user reported 9 cached / 29 in / 170 out,
	// where the cache info lives under `message.usage` rather than at
	// the top level on a message_start event.
	data := []byte(`{"type":"message_start","message":{"id":"chatcmpl-5plLbmztnQB65MvkpHx4JUwYd4PNFSzA","type":"message","role":"assistant","content":[],"model":"qwen","stop_reason":null,"stop_sequence":null,"usage":{"cache_read_input_tokens":9,"input_tokens":29,"output_tokens":0}}}`)
	u, ok := UsageFromResponseBody(data)
	if !ok || u == nil {
		t.Fatalf("expected usage block from message_start event")
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 9 {
		t.Errorf("cache_read_input_tokens: got %v, want 9", u.CacheReadTokens)
	}
	if u.InputTokens == nil || *u.InputTokens != 29 {
		t.Errorf("input_tokens: got %v, want 29", u.InputTokens)
	}
}

// TestUsageBlock_Merge_StreamingSparsely is the core regression: the
// Anthropic streaming protocol spreads the final usage across two
// events. message_start reports input + cache_read; message_delta
// reports output (and possibly cache_creation). The Merge method must
// not blank out fields from the earlier event when the later one
// omits them.
func TestUsageBlock_Merge_StreamingSparsely(t *testing.T) {
	first := &UsageBlock{
		InputTokens:    ptrInt(29),
		CacheReadTokens: ptrInt(9),
	}
	second := &UsageBlock{
		OutputTokens: ptrInt(170),
	}
	first.Merge(second)
	if first.InputTokens == nil || *first.InputTokens != 29 {
		t.Errorf("Merge clobbered input_tokens: got %v, want 29", first.InputTokens)
	}
	if first.CacheReadTokens == nil || *first.CacheReadTokens != 9 {
		t.Errorf("Merge clobbered cache_read_input_tokens: got %v, want 9", first.CacheReadTokens)
	}
	if first.OutputTokens == nil || *first.OutputTokens != 170 {
		t.Errorf("Merge did not pick up output_tokens: got %v, want 170", first.OutputTokens)
	}
}

// TestUsageBlock_CacheHitRate_Anthropic verifies the cache-hit rate is
// read straight from the same fields the inspect view's UI shows, so
// the Logs tab and the inspect view always agree.
func TestUsageBlock_CacheHitRate_Anthropic(t *testing.T) {
	cases := []struct {
		name  string
		block *UsageBlock
		want  float64
		ok    bool
	}{
		{
			name: "9 of 38 are cached → 24%",
			block: &UsageBlock{
				InputTokens:    ptrInt(29),
				CacheReadTokens: ptrInt(9),
			},
			want: 9.0 / 38.0,
			ok:   true,
		},
		{
			name: "100% cached",
			block: &UsageBlock{
				InputTokens:     ptrInt(0),
				CacheReadTokens: ptrInt(100),
			},
			want: 1.0,
			ok:   true,
		},
		{
			name: "0% cached (but info present)",
			block: &UsageBlock{
				InputTokens:    ptrInt(100),
				CacheReadTokens: ptrInt(0),
			},
			want: 0.0,
			ok:   true,
		},
		{
			name:  "nil block → no info",
			block: nil,
			ok:    false,
		},
		{
			name: "no cache fields → no info",
			block: &UsageBlock{
				InputTokens:  ptrInt(50),
				OutputTokens: ptrInt(10),
			},
			ok: false,
		},
		{
			name: "total prompt size zero (all zero) → no info",
			block: &UsageBlock{
				InputTokens:     ptrInt(0),
				CacheReadTokens: ptrInt(0),
			},
			ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.block.CacheHitRate()
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if absDiff(*got, tc.want) > 1e-4 {
				t.Fatalf("rate = %v, want %v", *got, tc.want)
			}
		})
	}
}

// TestUsageBlock_CacheHitRate_OpenAI verifies the OpenAI prompt_tokens +
// cached_tokens shape gives the same rate as the corresponding Anthropic
// shape.
func TestUsageBlock_CacheHitRate_OpenAI(t *testing.T) {
	u := &UsageBlock{
		InputTokens:  ptrInt(100),
		CachedTokens: ptrInt(91),
	}
	got, ok := u.CacheHitRate()
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if absDiff(*got, 0.91) > 1e-4 {
		t.Fatalf("rate = %v, want 0.91", *got)
	}
}

func TestRequestLogEntry_UsageJSONMarshaling(t *testing.T) {
	// Inspector off: no usage field on the wire.
	e := RequestLogEntry{Status: 200}
	data := mustMarshal(t, e)
	if contains(data, `"usage"`) {
		t.Fatalf("expected usage to be omitted, got %s", data)
	}

	// Inspector on, with usage.
	e.Usage = &UsageBlock{
		InputTokens:    ptrInt(29),
		CacheReadTokens: ptrInt(9),
	}
	data = mustMarshal(t, e)
	if !contains(data, `"input_tokens":29`) {
		t.Fatalf("expected input_tokens field, got %s", data)
	}
	if !contains(data, `"cache_read_input_tokens":9`) {
		t.Fatalf("expected cache_read_input_tokens field, got %s", data)
	}
}

func mustMarshal(t *testing.T, e RequestLogEntry) string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// abs and contains are shared helpers used across the engine test
// suite, including the cache_integration_test.go file that imports
// them transitively.
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
