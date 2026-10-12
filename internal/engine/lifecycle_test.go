package engine

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/jsplugins"
)

type resetFailureRuntime struct {
	*fakeExternal
	resetErr error
	releases int
}

func (r *resetFailureRuntime) Reset() error    { r.resets++; return r.resetErr }
func (r *resetFailureRuntime) ReleaseBuffers() { r.releases++ }

func TestPluginStreamFailureReachesAnthropicClient(t *testing.T) {
	for _, afterContent := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_content", true: "after_content"}[afterContent], func(t *testing.T) {
			s := newTestStore(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"local\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			addDownstream(t, s, "failure-ds", "failure-ds", server.URL, "test-key", "anthropic")
			addOutputModelIDs(t, s, "failure-ds", "failure-model")
			addRule(t, s, "failure-rule", "failure", "*", "", "failure-ds", `[{"plugin_id":"test/failure"}]`, true)
			eng := New(s)
			defer eng.Stop()
			ends, finishes, calls := 0, 0, 0
			f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
				"streamEvent": func(map[string]any) (map[string]any, error) {
					calls++
					if afterContent && calls == 1 {
						return nil, nil
					}
					return nil, &jsplugins.InvocationError{Code: "plugin_failure", Plugin: "test/failure@1.0.0", Hook: "streamEvent", Cause: errors.New("PRIVATE_PAYLOAD")}
				},
				"streamEnd": func(map[string]any) (map[string]any, error) { ends++; return nil, nil },
				"finish":    func(map[string]any) (map[string]any, error) { finishes++; return nil, nil },
			}}
			eng.SetRegistry(finishPublicationRegistry{NewExternalPlugin(f)})
			w := httptest.NewRecorder()
			eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"failure-model","stream":true,"messages":[]}`)))
			body := w.Body.String()
			want := http.StatusBadGateway
			if afterContent {
				want = http.StatusOK
			}
			if w.Code != want || strings.Contains(body, "PRIVATE_PAYLOAD") || strings.Contains(body, "event: message_stop") || !strings.Contains(body, "plugin_failure") {
				t.Fatalf("status=%d body=%s", w.Code, body)
			}
			if afterContent && (!strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"api_error"`)) {
				t.Fatalf("missing Anthropic error: %s", body)
			}
			entries := eng.GetLogger().RecentEntries(10)
			if len(entries) != 1 || entries[0].Error != "plugin stream error" {
				t.Fatalf("wrong logs: %+v", entries)
			}
			if ends != 1 || finishes != 1 {
				t.Fatalf("cleanup ends=%d finishes=%d", ends, finishes)
			}
		})
	}
}

func TestPartialAttemptInitializationCleanup(t *testing.T) {
	primary := errors.New("reset failed")
	var finishes [3]int
	var runtimes []*resetFailureRuntime
	var steps []*ExternalPlugin
	for n := 0; n < 3; n++ {
		index := n
		r := &resetFailureRuntime{fakeExternal: &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
			"finish":    func(map[string]any) (map[string]any, error) { finishes[index]++; return nil, nil },
			"streamEnd": func(map[string]any) (map[string]any, error) { t.Fatal("stale session cleanup"); return nil, nil },
		}}}
		runtimes = append(runtimes, r)
		step := NewExternalPlugin(r)
		step.begun = true
		step.stream.Start()
		steps = append(steps, step)
	}
	runtimes[1].resetErr = primary
	p := &Pipeline{ExternalSteps: steps, StreamResponseSteps: []StreamResponseTransformer{externalStream{p: steps[2]}}}
	ctx := &PipelineContext{}
	if err := p.beginAttempt(ctx, httptest.NewRequest("POST", "/", nil), 1); !errors.Is(err, primary) {
		t.Fatal(err)
	}
	if err := p.finishAttempt(ctx, "error"); err != nil {
		t.Fatal(err)
	}
	if err := p.finishAttempt(ctx, "error"); err != nil {
		t.Fatal(err)
	}
	if finishes != [3]int{1, 0, 0} || runtimes[2].resets != 0 {
		t.Fatal(finishes, runtimes[2].resets)
	}
	for _, r := range runtimes {
		if r.releases == 0 {
			t.Fatal("buffers not released")
		}
	}
	if !errors.Is(ctx.execution().Failure, primary) {
		t.Fatal("primary failure lost")
	}
}

func TestResponseHeadersObserveOriginalContentTypeAndClassification(t *testing.T) {
	const original = "text/event-stream; charset=utf-8"
	seen := ""
	runtime := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"responseHeaders": func(in map[string]any) (map[string]any, error) {
			seen = in["response"].(map[string]any)["headers"].(map[string]any)["content-type"].([]string)[0]
			return map[string]any{"response": map[string]any{"setHeaders": map[string]any{"Content-Type": []any{"application/json"}}}}, nil
		},
	}}
	step := NewExternalPlugin(runtime)
	step.request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := &PipelineContext{DownstreamFormat: "openai"}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{original}}}
	ctx.observeResponse(resp, true)
	if err := (&Pipeline{ExternalSteps: []*ExternalPlugin{step}}).responseHeaders(resp, ctx); err != nil {
		t.Fatal(err)
	}
	ctx.observeResponse(resp, isEventStream(original))
	if seen != original {
		t.Fatalf("responseHeaders saw content type %q, want original %q", seen, original)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("hook did not mutate response header: %q", got)
	}
	if got := ctx.execution().Observed; got != "sse" {
		t.Fatalf("classification followed mutated header: observed %q, want sse", got)
	}
}

func TestResponseHeaderFailureReachesFinish(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "exception", true: "invalid_effect"}[invalid], func(t *testing.T) {
			primary := &jsplugins.InvocationError{Code: "plugin_failure", Plugin: "test/header@2.0.0", Hook: "responseHeaders", Cause: errors.New("private cause")}
			var outcome map[string]any
			r := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
				"responseHeaders": func(map[string]any) (map[string]any, error) {
					if invalid {
						return map[string]any{"response": map[string]any{"setHeaders": map[string]any{"authorization": []any{"private"}}}}, nil
					}
					return nil, primary
				},
				"finish": func(in map[string]any) (map[string]any, error) {
					outcome = in["outcome"].(map[string]any)
					return nil, nil
				},
			}}
			p := &Pipeline{ExternalSteps: []*ExternalPlugin{NewExternalPlugin(r)}}
			ctx := &PipelineContext{}
			if err := p.beginAttempt(ctx, httptest.NewRequest("POST", "/", nil), 0); err != nil {
				t.Fatal(err)
			}
			if err := p.responseHeaders(&http.Response{Header: http.Header{}}, ctx); err == nil {
				t.Fatal("failure accepted")
			}
			if err := p.finishAttempt(ctx, "error"); err != nil {
				t.Fatal(err)
			}
			detail := outcome["error"].(map[string]any)
			code := "plugin_failure"
			if invalid {
				code = "invalid_effect"
			}
			if outcome["reason"] != "error" || detail["code"] != code || detail["hook"] != "responseHeaders" || detail["stage"] != "response_pipeline" {
				t.Fatal(outcome)
			}
			if !invalid && !errors.Is(ctx.execution().Failure, primary) {
				t.Fatal("cause lost")
			}
		})
	}
}

func TestSSEFullEnvelopeBounds(t *testing.T) {
	id, retry := "identity", 123
	event := SSEChunk{EventType: "message", Data: []byte("a\nb\n"), ID: &id, Retry: &retry}
	size := len(serializeEvents([]SSEChunk{event}))
	if serializedEventSize(event) != size {
		t.Fatal(serializedEventSize(event), size)
	}
	patch := map[string]any{"events": []any{map[string]any{"type": event.EventType, "data": string(event.Data), "id": id, "retry": float64(retry)}}}
	if _, err := eventResultLimit(patch, nil, size); err != nil {
		t.Fatal(err)
	}
	if _, err := eventResultLimit(patch, nil, size-1); err == nil {
		t.Fatal("wire overflow accepted")
	}
	// Metadata and multiline framing cannot hide behind an empty/tiny data field.
	for _, out := range []map[string]any{
		{"type": strings.Repeat("x", externalValueLimit), "data": ""},
		{"type": "", "data": "", "id": strings.Repeat("x", externalValueLimit)},
		{"type": "", "data": strings.Repeat("\n", externalValueLimit/7)},
	} {
		if _, err := eventResult(map[string]any{"events": []any{out}}, nil); err == nil {
			t.Fatal("envelope overflow accepted")
		}
	}
	if _, err := eventResultLimit(nil, []SSEChunk{event}, size-1); err == nil {
		t.Fatal("fallback envelope not bounded")
	}
	makeEmitter := func() *ExternalPlugin {
		return NewExternalPlugin(&fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
			"streamStart": func(map[string]any) (map[string]any, error) {
				return map[string]any{"events": []any{map[string]any{"type": "", "data": "", "id": strings.Repeat("x", externalValueLimit/2)}}}, nil
			},
		}})
	}
	// Two individually valid start emissions must share the composed limit.
	a, b := makeEmitter(), makeEmitter()
	a.request = httptest.NewRequest("POST", "/", nil)
	b.request = a.request
	if _, err := eventLifecycle([]StreamResponseTransformer{externalStream{p: a}, externalStream{p: b}}, "streamStart", "", &http.Response{Header: http.Header{}}, &PipelineContext{}); err == nil {
		t.Fatal("composed lifecycle overflow accepted")
	}
}

func TestUpstreamCollectionCompositionAndCleanup(t *testing.T) {
	var seen []string
	transform := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"upstreamStart": func(in map[string]any) (map[string]any, error) {
			seen = append(seen, "start")
			return map[string]any{"events": []any{fakeEvent("initial")}}, nil
		},
		"upstreamEvent": func(in map[string]any) (map[string]any, error) {
			return map[string]any{"events": []any{fakeEvent("changed")}}, nil
		},
		"upstreamEnd": func(in map[string]any) (map[string]any, error) {
			seen = append(seen, "end")
			return map[string]any{"events": []any{fakeEvent("tail")}}, nil
		},
	}}
	collector := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"collectStart": func(in map[string]any) (map[string]any, error) { seen = append(seen, "collectStart"); return nil, nil },
		"collectEvent": func(in map[string]any) (map[string]any, error) {
			seen = append(seen, in["event"].(map[string]any)["data"].(string))
			return nil, nil
		},
		"collectEnd": func(in map[string]any) (map[string]any, error) {
			seen = append(seen, "collectEnd")
			return map[string]any{"body": payload([]byte(`{"ok":true}`))}, nil
		},
	}}
	a, c := NewExternalPlugin(transform), NewExternalPlugin(collector)
	ctx := &PipelineContext{DownstreamFormat: "openai", ExternalCollector: c}
	c.request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	a.request = c.request
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: source\n\n"))}
	body, err := collectExternalPipeline(resp, ctx, &Pipeline{UpstreamSteps: []StreamResponseTransformer{externalStream{p: a, upstream: true}}})
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatal(string(body), err)
	}
	if strings.Join(seen, ",") != "collectStart,start,initial,changed,end,tail,collectEnd" {
		t.Fatal(seen)
	}
}

func TestStartFailureEndsEnteredParticipantsOnce(t *testing.T) {
	count := 0
	f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"upstreamStart": func(map[string]any) (map[string]any, error) { return nil, errors.New("secret credential") }, "upstreamEnd": func(in map[string]any) (map[string]any, error) { count++; return nil, nil }}}
	p := NewExternalPlugin(f)
	p.request = httptest.NewRequest("POST", "/", nil)
	ctx := &PipelineContext{}
	steps := []StreamResponseTransformer{externalStream{p: p, upstream: true}}
	_, err := eventLifecycle(steps, "upstreamStart", "", &http.Response{Header: http.Header{}}, ctx)
	if err == nil {
		t.Fatal("start accepted")
	}
	for n := 0; n < 2; n++ {
		_, _ = eventLifecycle(steps, "upstreamEnd", "error", nil, ctx)
	}
	if count != 1 {
		t.Fatal(count)
	}
	termination := ctx.termination("error")
	if strings.Contains(termination["error"].(map[string]any)["message"].(string), "secret") {
		t.Fatal(termination)
	}
}

func TestDeclaredTransportAndLargeResponseContext(t *testing.T) {
	ctx := &PipelineContext{ClientFormat: "openai", DownstreamFormat: "anthropic"}
	ctx.execution().Requested = "sse"
	ctx.ExternalCollector = NewExternalPlugin(&fakeExternal{})
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}}
	ctx.observeResponse(resp, true)
	snapshot := ctx.invocationContext()
	transport := snapshot["transport"].(map[string]any)
	if transport["observed"] != "sse" || transport["classification"] != "declared" || transport["reportedContentType"] != "application/json" || !ctx.execution().Large {
		t.Fatal(snapshot)
	}
	ctx.observeResponse(&http.Response{StatusCode: 429, Header: http.Header{}}, false)
	if ctx.execution().Delivery != "buffered" {
		t.Fatal("non2xx collected")
	}
}

func TestTruncatedCollectionHasStructuredCleanup(t *testing.T) {
	var ends int
	f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"collectStart": func(map[string]any) (map[string]any, error) { return nil, nil },
		"collectEnd": func(in map[string]any) (map[string]any, error) {
			ends++
			end := in["end"].(map[string]any)
			if end["reason"] != "error" || end["error"].(map[string]any)["category"] != "truncated_event" {
				t.Fatal(end)
			}
			return nil, nil
		},
	}}
	owner := NewExternalPlugin(f)
	owner.request = httptest.NewRequest("POST", "/", nil)
	ctx := &PipelineContext{ExternalCollector: owner}
	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: incomplete"))}
	if _, err := collectExternal(resp, ctx); err == nil {
		t.Fatal("truncation accepted")
	}
	if ends != 1 {
		t.Fatal(ends)
	}
}

func TestUnknownTransformClearsFormat(t *testing.T) {
	ctx := &PipelineContext{ClientFormat: "openai"}
	advanceFormat(ctx, struct{}{}, true)
	if ctx.execution().CurrentFormat != "" {
		t.Fatal("unknown transform retained claimed format")
	}
}

func TestCleanupContinuesAfterInvalidOutput(t *testing.T) {
	count := 0
	bad := NewExternalPlugin(&fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"streamEnd": func(map[string]any) (map[string]any, error) { return map[string]any{"events": "invalid"}, nil }}})
	good := NewExternalPlugin(&fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"streamEnd": func(map[string]any) (map[string]any, error) { count++; return nil, nil }}})
	bad.stream.Start()
	good.stream.Start()
	_, err := eventLifecycle([]StreamResponseTransformer{externalStream{p: bad}, externalStream{p: good}}, "streamEnd", "error", nil, &PipelineContext{})
	if err == nil || count != 1 {
		t.Fatal(err, count)
	}
}
