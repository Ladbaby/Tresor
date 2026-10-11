package engine

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/inspect"
)

type finishPublicationRegistry struct{ plugin *ExternalPlugin }

func (r finishPublicationRegistry) CreatePlugin(string, map[string]interface{}) (interface{}, error) {
	return r.plugin, nil
}
func (finishPublicationRegistry) ListPlugins() []PluginInfo { return nil }

func TestEngine_FinishOncePerRetryBeforePublication(t *testing.T) {
	s := newTestStore(t)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			io.WriteString(w, `{"choices":[]}`)
		} else {
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}
	}))
	defer server.Close()
	addDownstream(t, s, "finish-ds", "finish-ds", server.URL, "test-key", "openai")
	addOutputModelIDs(t, s, "finish-ds", "finish-model")
	addRule(t, s, "finish-rule", "Finish order", "*", "", "finish-ds", `[{"plugin_id":"test/finish","config":{}}]`, true)
	eng := New(s)
	defer eng.Stop()
	var outcomes []string
	f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"finish": func(input map[string]any) (map[string]any, error) {
			if len(eng.GetLogger().RecentEntries(10)) != 0 {
				t.Error("request published before cleanup")
			}
			outcomes = append(outcomes, input["outcome"].(map[string]any)["reason"].(string))
			return nil, nil
		},
	}}
	eng.SetRegistry(finishPublicationRegistry{NewExternalPlugin(f)})
	eng.SetRetryOnEmpty(true)
	w := httptest.NewRecorder()
	eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"finish-model","messages":[]}`)))
	if w.Code != 200 || strings.Join(outcomes, ",") != "retry,success" || f.resets != 2 {
		t.Fatalf("status=%d outcomes=%v resets=%d body=%s", w.Code, outcomes, f.resets, w.Body.String())
	}
	entries := eng.GetLogger().RecentEntries(10)
	if len(entries) != 1 {
		t.Fatal("missing final publication")
	}
	if entries[0].PluginDiagnostics == nil {
		t.Fatal("cleanup diagnostics missing from published request")
	}
	var finishAttempts []int
	for _, d := range entries[0].PluginDiagnostics.Entries {
		if d.Hook == "finish" {
			finishAttempts = append(finishAttempts, d.Attempt)
		}
	}
	if len(finishAttempts) != 2 || finishAttempts[0] != 0 || finishAttempts[1] != 1 {
		t.Fatalf("finish diagnostics: %v", finishAttempts)
	}
}

func TestEngine_HeaderMutationPreservesOriginalTransportDispatch(t *testing.T) {
	s := newTestStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	addDownstream(t, s, "headers-ds", "headers-ds", server.URL, "test-key", "openai")
	addOutputModelIDs(t, s, "headers-ds", "headers-model")
	addRule(t, s, "headers-rule", "Header observation", "*", "", "headers-ds", `[{"plugin_id":"test/headers","config":{}}]`, true)
	eng := New(s)
	defer eng.Stop()
	headerCalls, eventCalls := 0, 0
	f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"responseHeaders": func(in map[string]any) (map[string]any, error) {
			headerCalls++
			transport := in["context"].(map[string]any)["transport"].(map[string]any)
			if transport["observed"] != "sse" || transport["reportedContentType"] != "text/event-stream" {
				t.Errorf("initial observation: %v", transport)
			}
			return map[string]any{"response": map[string]any{"setHeaders": map[string]any{"content-type": []any{"application/json"}}}}, nil
		},
		"streamEvent": func(in map[string]any) (map[string]any, error) {
			eventCalls++
			transport := in["context"].(map[string]any)["transport"].(map[string]any)
			if transport["observed"] != "sse" || transport["reportedContentType"] != "application/json" {
				t.Errorf("dispatch observation: %v", transport)
			}
			return nil, nil
		},
	}}
	eng.SetRegistry(finishPublicationRegistry{NewExternalPlugin(f)})
	w := httptest.NewRecorder()
	eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"headers-model","stream":true}`)))
	if w.Code != 200 || headerCalls != 1 || eventCalls == 0 || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status=%d headers=%d events=%d body=%s", w.Code, headerCalls, eventCalls, w.Body.String())
	}
}

func TestEngine_CollectorCaptureRetainsWireSSE(t *testing.T) {
	s := newTestStore(t)
	const wire = "data: source\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
	}))
	defer server.Close()
	addDownstream(t, s, "collect-ds", "collect-ds", server.URL, "test-key", "openai")
	addOutputModelIDs(t, s, "collect-ds", "collect-model")
	addRule(t, s, "collect-rule", "Collector", "*", "", "collect-ds", `[{"plugin_id":"test/collector","config":{}}]`, true)
	eng := New(s)
	defer eng.Stop()
	capture := inspect.New(10)
	eng.SetPayloadStore(capture)
	f := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){
		"request": func(map[string]any) (map[string]any, error) {
			return map[string]any{"transport": map[string]any{"delivery": "collect", "upstream": "sse"}}, nil
		},
		"collectStart": func(map[string]any) (map[string]any, error) { return nil, nil },
		"collectEvent": func(map[string]any) (map[string]any, error) { return nil, nil },
		"collectEnd": func(map[string]any) (map[string]any, error) {
			return map[string]any{"body": payload([]byte(`{"choices":[{"message":{"content":"collected"}}]}`))}, nil
		},
	}}
	eng.SetRegistry(finishPublicationRegistry{NewExternalPlugin(f)})
	w := httptest.NewRecorder()
	eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"collect-model"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "collected") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	entries := eng.GetLogger().RecentEntries(1)
	if len(entries) != 1 {
		t.Fatal("missing log")
	}
	entry, ok := capture.Get(entries[0].ID)
	if !ok || string(entry.ResponseBody) != wire || entry.ResponseContentType != "text/event-stream" {
		t.Fatalf("unexpected raw capture: %+v", entry)
	}
}

type fakeExternal struct {
	hooks  map[string]func(map[string]any) (map[string]any, error)
	resets int
	calls  []string
}

func (f *fakeExternal) HasHook(name string) bool { return f.hooks[name] != nil }
func (f *fakeExternal) Reset() error             { f.resets++; return nil }
func (f *fakeExternal) PluginName() string       { return "test/fake@1.0.0" }
func (f *fakeExternal) Invoke(name string, input map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, name)
	return f.hooks[name](input)
}
func TestExternalRequestAtomicAndSingleSource(t *testing.T) {
	req := httptest.NewRequest("POST", "http://localhost/old?x=1&x=2", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer secret")
	ctx := &PipelineContext{DownstreamFormat: "anthropic", TargetDownstream: &Downstream{FormatPaths: map[string]string{"anthropic": "/configured"}}}
	runtime := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"request": func(input map[string]any) (map[string]any, error) {
		snapshot := input["request"].(map[string]any)
		if snapshot["path"] != "/configured" || snapshot["query"] != "x=1&x=2" {
			t.Fatal(snapshot)
		}
		if _, ok := snapshot["headers"].(map[string]any)["authorization"]; ok {
			t.Fatal("credential leaked")
		}
		if _, ok := input["operation"]; ok {
			t.Fatal("duplicate derived input")
		}
		return map[string]any{"body": payload([]byte("changed")), "request": map[string]any{"path": "//evil", "setHeaders": map[string]any{"x-test": []any{"yes"}}}}, nil
	}}}
	plugin := NewExternalPlugin(runtime)
	if _, _, err := plugin.transformRequest(req, []byte("body"), ctx); err == nil {
		t.Fatal("unsafe patch accepted")
	}
	if req.Header.Get("X-Test") != "" || ctx.RequestPathOverride != "" {
		t.Fatal("invalid patch applied partially")
	}
	runtime.hooks["request"] = func(map[string]any) (map[string]any, error) {
		return map[string]any{"request": map[string]any{"path": "/effective", "query": "", "setHeaders": map[string]any{"x-test": []any{"a", "b"}}}}, nil
	}
	out, body, err := plugin.transformRequest(req, []byte("body"), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "body" || out.URL.RawQuery != "" || ctx.RequestPathOverride != "/effective" || len(out.Header.Values("X-Test")) != 2 {
		t.Fatal(out, ctx)
	}
}
func fakeEvent(data string) map[string]any {
	return map[string]any{"type": "", "data": data, "id": nil, "retry": nil}
}
func TestExternalEventsAndOrderedFinalization(t *testing.T) {
	first := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"streamEvent": func(input map[string]any) (map[string]any, error) {
		if input["event"].(map[string]any)["data"] == "drop" {
			return map[string]any{"events": []any{}}, nil
		}
		return map[string]any{"events": []any{fakeEvent("one"), fakeEvent("two")}}, nil
	}, "streamEnd": func(map[string]any) (map[string]any, error) {
		return map[string]any{"events": []any{fakeEvent("tail")}}, nil
	}}}
	var seen []string
	second := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"streamEvent": func(input map[string]any) (map[string]any, error) {
		seen = append(seen, input["event"].(map[string]any)["data"].(string))
		return nil, nil
	}, "streamEnd": func(map[string]any) (map[string]any, error) { seen = append(seen, "end"); return nil, nil }}}
	steps := []StreamResponseTransformer{externalStream{p: NewExternalPlugin(first)}, externalStream{p: NewExternalPlugin(second)}}
	ctx := &PipelineContext{}
	out, err := executeEventList([]SSEChunk{{Data: []byte("input")}, {Data: []byte("drop")}}, ctx, steps)
	if err != nil || len(out) != 2 || strings.Join(seen, ",") != "one,two" {
		t.Fatal(out, seen, err)
	}
	for _, step := range steps {
		step.(externalStream).p.stream.Start()
	}
	p := &Pipeline{StreamResponseSteps: steps}
	trailing, err := p.streamLifecycle("streamEnd", "eof", nil, ctx)
	if err != nil || len(trailing) != 1 || strings.Join(seen, ",") != "one,two,tail,end" {
		t.Fatal(seen, err)
	}
	if _, err := p.streamLifecycle("streamEnd", "eof", nil, ctx); err != nil || strings.Join(seen, ",") != "one,two,tail,end" {
		t.Fatal("end ran twice", seen, err)
	}
	for _, step := range steps {
		step.(externalStream).p.stream = Session{Started: true}
	}
	if _, err := p.streamLifecycle("streamEnd", "cancelled", nil, ctx); err == nil {
		t.Fatal("cleanup emitted wire events")
	}
}
func TestExternalCollectorAndFinishIsolation(t *testing.T) {
	count := 0
	runtime := &fakeExternal{hooks: map[string]func(map[string]any) (map[string]any, error){"request": func(map[string]any) (map[string]any, error) {
		return map[string]any{"transport": map[string]any{"delivery": "collect"}}, nil
	}, "collectStart": func(map[string]any) (map[string]any, error) { count = 0; return nil, nil }, "collectEvent": func(map[string]any) (map[string]any, error) { count++; return nil, nil }, "collectEnd": func(input map[string]any) (map[string]any, error) {
		if input["end"].(map[string]any)["reason"] != "eof" || count != 2 {
			return nil, errors.New("incomplete")
		}
		return map[string]any{"body": payload([]byte(`{"collected":true}`))}, nil
	}, "finish": func(input map[string]any) (map[string]any, error) {
		if _, ok := input["body"]; ok {
			t.Fatal("finish received stale body")
		}
		return nil, nil
	}}}
	plugin := NewExternalPlugin(runtime)
	p := &Pipeline{ExternalSteps: []*ExternalPlugin{plugin}}
	ctx := &PipelineContext{}
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", nil)
	if err := p.beginAttempt(ctx, req, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plugin.transformRequest(req, nil, ctx); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: a\n\ndata: b\n\n"))}
	body, err := collectExternal(resp, ctx)
	if err != nil || string(body) != `{"collected":true}` {
		t.Fatal(string(body), err)
	}
	p.finishAttempt(ctx, "retry")
	p.finishAttempt(ctx, "error")
	if len(runtime.calls) == 0 || runtime.calls[len(runtime.calls)-1] != "finish" {
		t.Fatal(runtime.calls)
	}
	finishes := 0
	for _, name := range runtime.calls {
		if name == "finish" {
			finishes++
		}
	}
	if finishes != 1 {
		t.Fatal(runtime.calls)
	}
	if err := p.beginAttempt(ctx, req, 1); err != nil {
		t.Fatal(err)
	}
	if ctx.ExternalCollector != nil || ctx.AttemptIndex != 1 || runtime.resets != 2 {
		t.Fatal(ctx)
	}
	ctx.ClientStreaming = true
	if _, _, err := plugin.transformRequest(req, nil, ctx); err == nil {
		t.Fatal("streaming client collection accepted")
	}
}
func TestSSELogicalFields(t *testing.T) {
	var events []SSEChunk
	err := scanEvents(strings.NewReader("id: identity\nretry: 42\nevent: named\ndata:a\ndata: b\n\n"), func(event SSEChunk) error { events = append(events, event); return nil })
	if err != nil || len(events) != 1 || string(events[0].Data) != "a\nb" || *events[0].ID != "identity" || *events[0].Retry != 42 {
		t.Fatal(events, err)
	}
	wire := string(serializeEvents(events))
	if !strings.Contains(wire, "data: a\ndata: b\n") {
		t.Fatal(wire)
	}
}
