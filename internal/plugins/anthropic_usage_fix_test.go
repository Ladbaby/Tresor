package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tresor/internal/engine"
)

// TestFixAnthropicUsage_TransformResponse_NoChange covers the happy path:
// the response already conforms to the Anthropic usage schema and the
// plugin returns the body unmodified (semantically equivalent after
// marshal/unmarshal).
func TestFixAnthropicUsage_TransformResponse_NoChange(t *testing.T) {
	p := &FixAnthropicUsage{}

	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":10}
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(newBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := got["usage"].(map[string]interface{})
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := usage[f]; !ok {
			t.Errorf("usage missing %s after no-change roundtrip", f)
		}
	}
}

// TestFixAnthropicUsage_TransformResponse_AddsMissingCacheFields mirrors
// the minimax payload: usage is present but the two cache token fields
// are missing entirely. Both should be added as 0.
func TestFixAnthropicUsage_TransformResponse_AddsMissingCacheFields(t *testing.T) {
	p := &FixAnthropicUsage{}

	body := []byte(`{
		"id":"msg_2","type":"message","role":"assistant",
		"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":0,"output_tokens":0,"service_tier":"standard"}
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(newBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := got["usage"].(map[string]interface{})

	if v, ok := usage[usageFieldCacheCre].(float64); !ok || v != 0 {
		t.Errorf("expected cache_creation_input_tokens=0, got %v (ok=%v)", usage[usageFieldCacheCre], ok)
	}
	if v, ok := usage[usageFieldCacheRd].(float64); !ok || v != 0 {
		t.Errorf("expected cache_read_input_tokens=0, got %v (ok=%v)", usage[usageFieldCacheRd], ok)
	}
	if v, _ := usage[usageFieldInput].(float64); v != 0 {
		t.Errorf("input_tokens should be preserved as 0, got %v", v)
	}
	if v, _ := usage[usageFieldOutput].(float64); v != 0 {
		t.Errorf("output_tokens should be preserved as 0, got %v", v)
	}
	// service_tier must survive the rewrite untouched.
	if usage["service_tier"] != "standard" {
		t.Errorf("service_tier was clobbered: %v", usage["service_tier"])
	}
}

// TestFixAnthropicUsage_TransformResponse_AddsMissingInput ensures the
// plugin fills in any of the four canonical fields when absent, including
// input_tokens and output_tokens which a misbehaving provider could omit.
func TestFixAnthropicUsage_TransformResponse_AddsMissingInput(t *testing.T) {
	p := &FixAnthropicUsage{}

	body := []byte(`{
		"id":"msg_3","type":"message","role":"assistant",
		"content":[{"type":"text","text":"x"}],
		"usage":{"cache_creation_input_tokens":1,"cache_read_input_tokens":2}
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var got map[string]interface{}
	json.Unmarshal(newBody, &got)
	usage := got["usage"].(map[string]interface{})
	if v, _ := usage[usageFieldInput].(float64); v != 0 {
		t.Errorf("input_tokens should be added as 0, got %v", v)
	}
	if v, _ := usage[usageFieldOutput].(float64); v != 0 {
		t.Errorf("output_tokens should be added as 0, got %v", v)
	}
	if v, _ := usage[usageFieldCacheCre].(float64); v != 1 {
		t.Errorf("cache_creation_input_tokens should be preserved as 1, got %v", v)
	}
	if v, _ := usage[usageFieldCacheRd].(float64); v != 2 {
		t.Errorf("cache_read_input_tokens should be preserved as 2, got %v", v)
	}
}

// TestFixAnthropicUsage_TransformResponse_NoUsageObject: responses without
// a usage block (e.g. error responses) must NOT be modified.
func TestFixAnthropicUsage_TransformResponse_NoUsageObject(t *testing.T) {
	p := &FixAnthropicUsage{}

	body := []byte(`{
		"id":"msg_err","type":"message","role":"assistant",
		"content":[]
	}`)

	resp := &http.Response{Header: http.Header{}}
	newBody, err := p.TransformResponse(resp, body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	if !bytes.Equal(body, newBody) {
		t.Errorf("body changed despite no usage block: %s", string(newBody))
	}
}

// TestFixAnthropicUsage_StreamMessageStartMissingCacheFields: the minimax
// stream. message_start is patched to include the two cache fields.
func TestFixAnthropicUsage_StreamMessageStartMissingCacheFields(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"m","model":"MiniMax-M3","usage":{"input_tokens":0,"output_tokens":0,"service_tier":"standard"}}}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if out.EventType != "message_start" {
		t.Errorf("event type changed: %s", out.EventType)
	}

	var got struct {
		Message struct {
			Usage map[string]interface{} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshal rewritten chunk: %v\n%s", err, string(out.Data))
	}
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := got.Message.Usage[f]; !ok {
			t.Errorf("message_start usage missing %s after rewrite: %s", f, string(out.Data))
		}
	}
}

// TestFixAnthropicUsage_StreamMessageDeltaSynthesizesUsage: after
// message_start records the start counts, a subsequent message_delta
// that lacks `usage` gets one synthesized.
func TestFixAnthropicUsage_StreamMessageDeltaSynthesizesUsage(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	// Feed message_start with input=45892 (a Deepseek-like real count).
	start := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"m","usage":{"input_tokens":45892,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`),
	}
	if _, err := p.TransformStreamChunk(start, ctx); err != nil {
		t.Fatalf("message_start transform: %v", err)
	}

	delta := engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`),
	}
	out, err := p.TransformStreamChunk(delta, ctx)
	if err != nil {
		t.Fatalf("message_delta transform: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshal delta: %v\n%s", err, string(out.Data))
	}
	usage, ok := got["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected usage to be synthesized, got: %s", string(out.Data))
	}
	if v, _ := usage[usageFieldInput].(float64); v != 45892 {
		t.Errorf("synthesized input_tokens should mirror start (45892), got %v", v)
	}
	for _, f := range []string{usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := usage[f]; !ok {
			t.Errorf("synthesized usage missing %s: %s", f, string(out.Data))
		}
	}
	// delta must be preserved verbatim.
	delta2, _ := got["delta"].(map[string]interface{})
	if delta2["stop_reason"] != "end_turn" {
		t.Errorf("delta.stop_reason was clobbered: %v", delta2)
	}
}

// TestFixAnthropicUsage_StreamMessageDeltaWithUsage: a conforming
// message_delta that already has a usage block must pass through
// unchanged (semantically — we round-trip through marshal).
func TestFixAnthropicUsage_StreamMessageDeltaWithUsage(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	// Deepseek-shaped delta with a populated usage.
	delta := engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":45892,"output_tokens":130,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`),
	}
	out, err := p.TransformStreamChunk(delta, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(out.Data, delta.Data) {
		t.Errorf("data changed unexpectedly: %s -> %s", string(delta.Data), string(out.Data))
	}
}

// TestFixAnthropicUsage_StreamNonUsageEvents: every event type other than
// message_start / message_delta / message_stop passes through unchanged.
func TestFixAnthropicUsage_StreamNonUsageEvents(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	events := []engine.SSEChunk{
		{EventType: "ping", Data: []byte(`{"type":"ping"}`)},
		{EventType: "content_block_start", Data: []byte(`{"index":0,"content_block":{"type":"text","text":""}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"index":0,"delta":{"type":"text_delta","text":"hi"}}`)},
		{EventType: "content_block_stop", Data: []byte(`{"index":0}`)},
	}
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform %s: %v", ev.EventType, err)
		}
		if out.EventType != ev.EventType || !bytes.Equal(out.Data, ev.Data) {
			t.Errorf("event %s was modified: in=%q out=%q", ev.EventType, string(ev.Data), string(out.Data))
		}
	}
}

// TestFixAnthropicUsage_StreamMessageStopResetsState: state recorded from
// message_start must not leak across synthetic stream boundaries.
func TestFixAnthropicUsage_StreamMessageStopResetsState(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	// First "stream": start with input=99, delta synthesizes 99.
	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"message":{"usage":{"input_tokens":99,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`),
	}, ctx)
	out1, _ := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"delta":{"stop_reason":"end_turn"}}`),
	}, ctx)
	var d1 map[string]interface{}
	json.Unmarshal(out1.Data, &d1)
	if v, _ := d1["usage"].(map[string]interface{})[usageFieldInput].(float64); v != 99 {
		t.Fatalf("first stream delta should carry input_tokens=99, got %v", v)
	}

	// message_stop resets state.
	p.TransformStreamChunk(engine.SSEChunk{EventType: "message_stop", Data: []byte(`{"type":"message_stop"}`)}, ctx)

	// Second "stream": no message_start observed yet — synthesized delta
	// should carry zeros (state was reset), not the stale 99.
	out2, _ := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"delta":{"stop_reason":"end_turn"}}`),
	}, ctx)
	var d2 map[string]interface{}
	json.Unmarshal(out2.Data, &d2)
	if v, _ := d2["usage"].(map[string]interface{})[usageFieldInput].(float64); v != 0 {
		t.Errorf("state leaked across streams: input_tokens=%v, want 0", v)
	}
}

// TestFixAnthropicUsage_EndToEndMinimaxTrace replays the captured minimax
// stream (from claude_code_response_minimax.txt). After the fix runs, the
// output must contain the two cache fields in message_start and a populated
// usage block in message_delta.
func TestFixAnthropicUsage_EndToEndMinimaxTrace(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	events := []engine.SSEChunk{
		{EventType: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"x","model":"MiniMax-M3","content":[],"usage":{"input_tokens":0,"output_tokens":0,"service_tier":"standard"},"service_tier":"standard"}}`)},
		{EventType: "ping", Data: []byte(`{"type":"ping"}`)},
		{EventType: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)},
		{EventType: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)},
		{EventType: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
		{EventType: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":""}}`)},
		{EventType: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}

	var outputs []engine.SSEChunk
	for _, ev := range events {
		out, err := p.TransformStreamChunk(ev, ctx)
		if err != nil {
			t.Fatalf("transform %s: %v", ev.EventType, err)
		}
		outputs = append(outputs, out)
	}

	// 1) message_start must carry all four canonical usage fields.
	var startGot struct {
		Message struct {
			Usage map[string]interface{} `json:"usage"`
		} `json:"message"`
	}
	json.Unmarshal(outputs[0].Data, &startGot)
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := startGot.Message.Usage[f]; !ok {
			t.Errorf("minimax replay: message_start missing %s; got %s", f, string(outputs[0].Data))
		}
	}

	// 2) message_delta (events index 5) must now carry a usage block.
	var deltaGot map[string]interface{}
	json.Unmarshal(outputs[5].Data, &deltaGot)
	usage, ok := deltaGot["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("minimax replay: message_delta missing synthesized usage; got %s", string(outputs[5].Data))
	}
	if _, ok := usage[usageFieldInput]; !ok {
		t.Errorf("minimax replay: synthesized usage missing input_tokens; got %v", usage)
	}
	// And delta.stop_reason must be preserved.
	if delta, _ := deltaGot["delta"].(map[string]interface{}); delta == nil || delta["stop_reason"] != "" {
		t.Errorf("minimax replay: delta.stop_reason lost; got %v", deltaGot["delta"])
	}

	// 3) Other events must be byte-identical (cheap sanity check on
	// pass-through behaviour).
	for i, ev := range []int{1, 2, 3, 4, 6} {
		if !bytes.Equal(outputs[ev].Data, events[ev].Data) {
			t.Errorf("event %d (%s) was modified", i, events[ev].EventType)
		}
		if outputs[ev].EventType != events[ev].EventType {
			t.Errorf("event %d type changed: %s -> %s", i, events[ev].EventType, outputs[ev].EventType)
		}
	}
}

// TestFixAnthropicUsage_StreamMessageStartPreservesEnvelopeFields asserts
// that a message_start round-trip preserves every top-level field the
// upstream sent, not just `message.usage`. A previous implementation
// decoded into a struct with only a `message` field, which silently
// dropped the top-level `type` discriminator (and any other top-level
// fields such as `service_tier`). Closed-source SDKs that read the JSON
// `type` field to confirm the event kind — including the Claude Code
// VS Code extension — would then reject the message_start and any
// following content_block_delta would arrive "without a current
// message". This is a regression guard for that bug.
func TestFixAnthropicUsage_StreamMessageStartPreservesEnvelopeFields(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	// Captured minimax stream. The payload has `type` and `service_tier`
	// at the top level in addition to the `message` envelope — all three
	// must survive the round-trip.
	chunk := engine.SSEChunk{
		EventType: "message_start",
		Data: []byte(`{"type":"message_start","message":{"id":"06a3db7ce6182830f962908c3f3e7666","type":"message","role":"assistant","content":[],"model":"MiniMax-M3","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"service_tier":"standard"},"service_tier":"standard"},"service_tier":"standard"}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Top-level type discriminator must be preserved.
	if got["type"] != "message_start" {
		t.Errorf("top-level `type` field was dropped: got %v, want \"message_start\"; data=%s", got["type"], string(out.Data))
	}
	// Top-level service_tier must be preserved (some providers echo it
	// at both top level and inside message.usage).
	if got["service_tier"] != "standard" {
		t.Errorf("top-level `service_tier` was dropped: got %v, want \"standard\"; data=%s", got["service_tier"], string(out.Data))
	}
	// The message envelope itself must still parse.
	msg, ok := got["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("`message` envelope was dropped: %s", string(out.Data))
	}
	// Inner usage must carry all four canonical fields.
	usage, ok := msg["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("`message.usage` was dropped: %s", string(out.Data))
	}
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := usage[f]; !ok {
			t.Errorf("message_start usage missing %s after rewrite: %s", f, string(out.Data))
		}
	}
}

// TestFixAnthropicUsage_StreamMessageStartPreservesArbitraryEnvelope is
// the broader version of the regression guard: an upstream that injects
// arbitrary top-level fields (e.g. custom metadata) must not have them
// silently dropped by the message_start patch.
func TestFixAnthropicUsage_StreamMessageStartPreservesArbitraryEnvelope(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"m","model":"M","usage":{"input_tokens":1,"output_tokens":2}},"custom_top":"keep_me","another":42}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "message_start" {
		t.Errorf("top-level `type` was dropped: got %v", got["type"])
	}
	if got["custom_top"] != "keep_me" {
		t.Errorf("custom top-level field was dropped: got %v", got["custom_top"])
	}
	if v, ok := got["another"].(float64); !ok || v != 42 {
		t.Errorf("custom numeric top-level field was dropped: got %v (ok=%v)", got["another"], ok)
	}
}

// TestFixAnthropicUsage_NormalizeUsageMap covers the helper directly so a
// future refactor that breaks it fails the test before integration tests
// do.
func TestFixAnthropicUsage_NormalizeUsageMap(t *testing.T) {
	// Empty map: all four fields should be added.
	m := map[string]interface{}{}
	if !normalizeUsageMap(m) {
		t.Fatal("expected change for empty map")
	}
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing %s after normalize", f)
		}
	}

	// Fully populated map: no change.
	full := map[string]interface{}{
		usageFieldInput:    1,
		usageFieldOutput:   2,
		usageFieldCacheCre: 3,
		usageFieldCacheRd:  4,
	}
	if normalizeUsageMap(full) {
		t.Fatal("fully populated map should report no change")
	}

	// Existing values must not be clobbered.
	mixed := map[string]interface{}{
		usageFieldInput:  42,
		usageFieldOutput: 7,
	}
	normalizeUsageMap(mixed)
	if mixed[usageFieldInput].(int) != 42 || mixed[usageFieldOutput].(int) != 7 {
		t.Errorf("existing values were clobbered: %v", mixed)
	}
	if _, ok := mixed[usageFieldCacheCre]; !ok {
		t.Error("missing fields were not added")
	}
}

// TestFixAnthropicUsage_StreamMessageStartNoUsageAtAll is the defensive
// path: a message_start whose message lacks any usage key at all still
// gets one synthesized (rather than leaving the field undefined, which is
// exactly what pi-agent crashes on).
func TestFixAnthropicUsage_StreamMessageStartNoUsageAtAll(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	chunk := engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"type":"message_start","message":{"id":"m","model":"MiniMax-M3"}}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got struct {
		Message struct {
			Usage map[string]interface{} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Message.Usage == nil {
		t.Fatalf("expected usage to be added; got %s", string(out.Data))
	}
	for _, f := range []string{usageFieldInput, usageFieldOutput, usageFieldCacheCre, usageFieldCacheRd} {
		if _, ok := got.Message.Usage[f]; !ok {
			t.Errorf("added usage missing %s: %s", f, string(out.Data))
		}
	}
}

// TestFixAnthropicUsage_StreamMessageDeltaUsesRecordedStart confirms the
// message_delta synthesis copies the values captured at message_start
// (a non-zero input_tokens count, in particular).
func TestFixAnthropicUsage_StreamMessageDeltaUsesRecordedStart(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_start",
		Data:      []byte(`{"message":{"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":3,"cache_read_input_tokens":7}}}`),
	}, ctx)

	out, err := p.TransformStreamChunk(engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"delta":{}}`),
	}, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got map[string]interface{}
	json.Unmarshal(out.Data, &got)
	usage := got["usage"].(map[string]interface{})
	if v, _ := usage[usageFieldInput].(float64); v != 100 {
		t.Errorf("input_tokens: got %v, want 100", v)
	}
	if v, _ := usage[usageFieldOutput].(float64); v != 50 {
		t.Errorf("output_tokens: got %v, want 50", v)
	}
	if v, _ := usage[usageFieldCacheCre].(float64); v != 3 {
		t.Errorf("cache_creation: got %v, want 3", v)
	}
	if v, _ := usage[usageFieldCacheRd].(float64); v != 7 {
		t.Errorf("cache_read: got %v, want 7", v)
	}
}

// Sanity: verify that the strings.Contains early-out in the stream
// transformer doesn't break JSON with "usage" appearing as a substring in
// an unrelated field name.
func TestFixAnthropicUsage_StreamMessageDeltaDoesNotTouchUnrelatedUsageMention(t *testing.T) {
	p := &FixAnthropicUsage{}
	ctx := &engine.PipelineContext{}

	// The string "usage" appears inside delta.usage_summary but not as a
	// top-level key. The early-out heuristic must not skip the rewrite in
	// this case — the chunk still lacks a top-level `usage` block and we
	// should inject one.
	chunk := engine.SSEChunk{
		EventType: "message_delta",
		Data:      []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","usage_summary":"ok"}}`),
	}
	out, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !strings.Contains(string(out.Data), `"usage":`) {
		t.Errorf("expected top-level usage to be synthesized; got %s", string(out.Data))
	}
}