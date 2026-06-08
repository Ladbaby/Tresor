package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"tresor/internal/store"
)

// mockRegistryImpl supports creating custom_header, pass-through, and translation plugins.
type mockRegistryImpl struct{}

func (m *mockRegistryImpl) CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error) {
	switch pluginID {
	case "custom_header":
		return &mockCustomHeader{Config: config}, nil
	case "pass_through":
		return &mockPassThrough{}, nil
	case "openai2anthropic":
		return &mockOpenAI2Anthropic{}, nil
	case "anthropic2openai":
		return &mockAnthropic2OpenAI{}, nil
	default:
		return nil, fmt.Errorf("unknown plugin: %s", pluginID)
	}
}

func (m *mockRegistryImpl) ListPlugins() []PluginInfo {
	return nil
}

// mockCustomHeader injects headers from config.
type mockCustomHeader struct {
	Config map[string]interface{}
}

func (m *mockCustomHeader) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	if headers, ok := m.Config["headers"].(map[string]interface{}); ok {
		for k, v := range headers {
			if vs, ok := v.(string); ok {
				req.Header.Set(k, vs)
			}
		}
	}
	return req, body, nil
}

// mockPassThrough does nothing to requests or responses.
type mockPassThrough struct{}

func (m *mockPassThrough) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	return req, body, nil
}

func (m *mockPassThrough) TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error) {
	return body, nil
}

// mockOpenAI2Anthropic marks the request body as translated.
type mockOpenAI2Anthropic struct{}

func (m *mockOpenAI2Anthropic) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	req.Header.Set("X-Auto-Translated", "openai2anthropic")
	return req, body, nil
}

func (m *mockOpenAI2Anthropic) TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error) {
	return body, nil
}

func (m *mockOpenAI2Anthropic) TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error) {
	return chunk, nil
}

// mockAnthropic2OpenAI marks the request body as translated.
type mockAnthropic2OpenAI struct{}

func (m *mockAnthropic2OpenAI) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	req.Header.Set("X-Auto-Translated", "anthropic2openai")
	return req, body, nil
}

func (m *mockAnthropic2OpenAI) TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error) {
	return body, nil
}

func (m *mockAnthropic2OpenAI) TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error) {
	return chunk, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	f, err := os.CreateTemp("", "tresor-engine-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	s, err := store.Open(f.Name())
	if err != nil {
		os.Remove(f.Name())
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		os.Remove(f.Name())
	})
	return s
}

// addDownstream creates a test downstream record.
func addDownstream(t *testing.T, s *store.Store, id, name, baseURL, apiKey string, apiFormats ...string) {
	t.Helper()
	formats := []string{}
	if len(apiFormats) > 0 {
		formats = apiFormats
	}
	if err := s.CreateDownstream(&store.Downstream{
		ID: id, Name: name, BaseURL: baseURL, APIKey: apiKey, ApiFormats: formats,
	}); err != nil {
		t.Fatalf("create downstream %s: %v", id, err)
	}
}

// addOutputModelIDs registers model IDs on a downstream so it can be resolved.
func addOutputModelIDs(t *testing.T, s *store.Store, downstreamID string, models ...string) {
	t.Helper()
	for _, m := range models {
		if err := s.AddOutputModelID(downstreamID, m); err != nil {
			t.Fatalf("add output model id %s for downstream %s: %v", m, downstreamID, err)
		}
	}
}

// addRule creates a test rule record. The downstream parameter populates
// match_downstreams (for filtering only — rules no longer override the target downstream).
func addRule(t *testing.T, s *store.Store, id, name, patternPath, patternModel, downstream, pipeline string, enabled bool) {
	t.Helper()
	md := []string{}
	if downstream != "" {
		md = []string{downstream}
	}
	if err := s.CreateRule(&store.Rule{
		ID: id, Name: name, PatternPath: patternPath, PatternModel: patternModel,
		MatchDownstreams: md, PipelineConfig: pipeline, IsEnabled: enabled,
	}); err != nil {
		t.Fatalf("create rule %s: %v", id, err)
	}
}

// newTestDownstream returns a test HTTP server that records the request and
// returns a canned response.
func newTestDownstream(t *testing.T, status int, body string, checkHeaders func(t *testing.T, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkHeaders != nil {
			checkHeaders(t, r)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestEngine_HandleProxy_UnknownModel_Returns404(t *testing.T) {
	s := newTestStore(t)
	ts := newTestDownstream(t, 200, `{}`, nil)
	defer ts.Close()
	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Request with a model not registered on any downstream
	body := `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(bytes.ToLower([]byte(string(respBody))), []byte("unknown")) {
		t.Fatalf("expected error mentioning unknown model, got: %s", string(respBody))
	}
}

func TestEngine_HandleProxy_WildcardMatch(t *testing.T) {
	s := newTestStore(t)

	var authHeader string
	ts := newTestDownstream(t, 200, `{"ok":true}`, func(t *testing.T, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
	})
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "claude-sonnet-4-20250514")
	addRule(t, s, "r1", "Wildcard", "*", "", "ds1", "[]", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
	if authHeader != "Bearer key-ds1" {
		t.Fatalf("expected Bearer key-ds1, got %q", authHeader)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result)
	}
}

func TestEngine_HandleProxy_PathOnlyMatch(t *testing.T) {
	s := newTestStore(t)

	var authHeader string
	ts := newTestDownstream(t, 200, `{"ok":true}`, func(t *testing.T, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
	})
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	addRule(t, s, "r1", "PathOnly", "/v1/chat/completions", "", "ds1", "[]", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Request matching the path - model resolves via output_model_ids
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
	if authHeader != "Bearer key-ds1" {
		t.Fatalf("expected Bearer key-ds1, got %q", authHeader)
	}
}

func TestEngine_HandleProxy_ModelPriority(t *testing.T) {
	s := newTestStore(t)

	var headerReceived string
	ts := newTestDownstream(t, 200, `{"ok":true}`, func(t *testing.T, r *http.Request) {
		headerReceived = r.Header.Get("X-Priority")
	})
	defer ts.Close()

	// Single downstream - model resolves here
	addDownstream(t, s, "ds1", "DS1", ts.URL, "key-1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")

	// Path-only rule (lower priority) - sets X-Priority: path-only
	addRule(t, s, "r1", "PathOnly", "/v1/chat/completions", "", "ds1", `[{"plugin_id":"custom_header","config":{"headers":{"X-Priority":"path-only"}}}]`, true)
	// Path+model rule (higher priority) - sets X-Priority: model-specific
	addRule(t, s, "r2", "ModelSpecific", "/v1/chat/completions", "gpt-4o", "ds1", `[{"plugin_id":"custom_header","config":{"headers":{"X-Priority":"model-specific"}}}]`, true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Both rules match (same downstream filter). Pipelines are concatenated in priority order:
	// model-specific first, then path-only. Since path-only runs second, it overwrites the header.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Path-only rule runs second, overwriting model-specific's header
	if headerReceived != "path-only" {
		t.Fatalf("expected X-Priority: path-only (both rules matched, path-only runs second), got %q", headerReceived)
	}
}

func TestEngine_HandleProxy_PipelineRuns(t *testing.T) {
	s := newTestStore(t)

	var headerReceived string
	ts := newTestDownstream(t, 200, `{"transformed":true}`, func(t *testing.T, r *http.Request) {
		headerReceived = r.Header.Get("X-Debug")
	})
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	addRule(t, s, "r1", "Debug", "*", "", "ds1",
		`[{"plugin_id":"custom_header","config":{"headers":{"X-Debug":"true"}}}]`, true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
	if headerReceived != "true" {
		t.Fatalf("expected X-Debug: true, got %q", headerReceived)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["transformed"] != true {
		t.Fatalf("expected transformed=true, got %v", result)
	}
}

func TestEngine_HandleProxy_AuthHeaderConflict(t *testing.T) {
	s := newTestStore(t)

	var apiKeyHeader, authHeader string
	ts := newTestDownstream(t, 200, `{"ok":true}`, func(t *testing.T, r *http.Request) {
		apiKeyHeader = r.Header.Get("x-api-key")
		authHeader = r.Header.Get("Authorization")
	})
	defer ts.Close()

	addDownstream(t, s, "ds1", "ds1", ts.URL, "default-key")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	// Pipeline sets x-api-key — engine should NOT add Bearer default-key
	addRule(t, s, "r1", "CustomAuth", "*", "", "ds1",
		`[{"plugin_id":"custom_header","config":{"headers":{"x-api-key":"override-key"}}}]`, true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
	// The downstream should see the pipeline-set x-api-key, NOT the default Bearer token
	if apiKeyHeader != "override-key" {
		t.Fatalf("expected x-api-key: override-key, got %q", apiKeyHeader)
	}
	// The default Authorization header should NOT have been added
	if authHeader == "Bearer default-key" {
		t.Fatal("engine should NOT set Bearer default-key when x-api-key was already set")
	}
}

// addAlias creates a test alias record.
func addAlias(t *testing.T, s *store.Store, inputModelID, downstreamID, outputModelID string, isActive bool) {
	t.Helper()
	if err := s.CreateAlias(&store.Alias{
		InputModelID: inputModelID, DownstreamID: downstreamID, OutputModelID: outputModelID, IsActive: isActive,
	}); err != nil {
		t.Fatalf("create alias %s->%s: %v", inputModelID, outputModelID, err)
	}
}

func TestEngine_HandleProxy_AliasOverridesDownstream(t *testing.T) {
	s := newTestStore(t)

	// Two downstream servers with different markers
	var ds1Auth string
	ds1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds1Auth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"from":"ds1"}`))
	}))
	defer ds1Server.Close()

	var ds2Auth string
	ds2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds2Auth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"from":"ds2"}`))
	}))
	defer ds2Server.Close()

	// ds1 is the rule's downstream (key-ds1), ds2 will be the alias's downstream (key-ds2)
	addDownstream(t, s, "rule-ds", "RuleDS", ds1Server.URL, "key-ds1")
	addOutputModelIDs(t, s, "rule-ds", "gpt-4o")
	addDownstream(t, s, "alias-ds", "AliasDS", ds2Server.URL, "key-ds2")

	// Rule points to rule-ds
	addRule(t, s, "r1", "Chat", "/v1/chat/completions", "", "rule-ds", "[]", true)

	// Alias for gpt-4o redirects to alias-ds with output model claude-sonnet
	addAlias(t, s, "gpt-4o", "alias-ds", "claude-sonnet", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	// Should have been routed to ds2 (alias-ds), not ds1 (rule-ds)
	if string(respBody) != `{"from":"ds2"}` {
		t.Fatalf("expected response from ds2, got: %s", string(respBody))
	}
	// Auth should be the alias-ds key, not rule-ds key
	if ds2Auth != "Bearer key-ds2" {
		t.Fatalf("expected Bearer key-ds2 (alias downstream), got %q", ds2Auth)
	}
	if ds1Auth != "" {
		t.Fatal("ds1 should NOT have received the request when alias is active")
	}
}

func TestEngine_HandleProxy_AliasRewritesModel(t *testing.T) {
	s := newTestStore(t)

	var forwardedBody string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwardedBody = string(b)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	addDownstream(t, s, "ds1", "DS1", dsServer.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	addRule(t, s, "r1", "Chat", "/v1/chat/completions", "", "ds1", "[]", true)
	// Alias rewrites gpt-4o -> claude-3.5
	addAlias(t, s, "gpt-4o", "ds1", "claude-3.5", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// The downstream should receive the rewritten model name
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(forwardedBody), &payload); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if payload["model"] != "claude-3.5" {
		t.Fatalf("expected model claude-3.5 in forwarded request, got %q", payload["model"])
	}
}

func TestEngine_HandleProxy_NoAlias_UsesRuleDownstream(t *testing.T) {
	s := newTestStore(t)

	var authHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	addDownstream(t, s, "ds1", "DS1", dsServer.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	addRule(t, s, "r1", "Chat", "/v1/chat/completions", "", "ds1", "[]", true)
	// No alias for gpt-4o — model resolves via output_model_ids, rule provides pipeline

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Should use the downstream resolved by output_model_ids (same as rule's downstream here)
	if authHeader != "Bearer key-ds1" {
		t.Fatalf("expected Bearer key-ds1, got %q", authHeader)
	}
}

// TestEngine_HandleProxy_AliasWithoutRule verifies that an alias alone is enough
// to forward a request — no rule is needed.
func TestEngine_HandleProxy_AliasWithoutRule(t *testing.T) {
	s := newTestStore(t)

	var authHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	addDownstream(t, s, "ds1", "DS1", dsServer.URL, "key-ds1")
	// No rule — alias alone should be enough to forward
	addAlias(t, s, "gpt-4o", "ds1", "gpt-4o", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (alias forwarding without rule), got %d", resp.StatusCode)
	}
	if authHeader != "Bearer key-ds1" {
		t.Fatalf("expected Bearer key-ds1, got %q", authHeader)
	}
}

// TestEngine_HandleProxy_DirectModelForwards verifies that a downstream's
// output_model_ids alone is enough to forward — no alias or rule needed.
func TestEngine_HandleProxy_DirectModelForwards(t *testing.T) {
	s := newTestStore(t)

	var authHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	addDownstream(t, s, "ds1", "DS1", dsServer.URL, "key-ds1")
	addOutputModelIDs(t, s, "ds1", "gpt-4o")
	// No alias, no rule — just downstream output_model_ids

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (direct model forwarding), got %d", resp.StatusCode)
	}
	if authHeader != "Bearer key-ds1" {
		t.Fatalf("expected Bearer key-ds1, got %q", authHeader)
	}
}

// TestEngine_HandleProxy_EmptyModel_Returns400 verifies that requests without
// a model field are rejected with 400 Bad Request.
func TestEngine_HandleProxy_EmptyModel_Returns400(t *testing.T) {
	s := newTestStore(t)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Body with no model field
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing model, got %d", resp.StatusCode)
	}
}

// TestEngine_HandleProxy_RuleDoesNotOverrideDownstream verifies that rules
// no longer override the resolved downstream. The downstream is determined
// solely by aliases or output_model_ids. Rules only contribute pipelines.
func TestEngine_HandleProxy_RuleDoesNotOverrideDownstream(t *testing.T) {
	s := newTestStore(t)

	var ds1Hit, ds2Hit bool
	ds1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds1Hit = true
		w.WriteHeader(200)
		w.Write([]byte(`{"from":"ds1"}`))
	}))
	defer ds1Server.Close()

	ds2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds2Hit = true
		w.WriteHeader(200)
		w.Write([]byte(`{"from":"ds2"}`))
	}))
	defer ds2Server.Close()

	// ds1 owns the model "gpt-4o" in output_model_ids
	addDownstream(t, s, "model-ds", "ModelDS", ds1Server.URL, "key-1")
	addOutputModelIDs(t, s, "model-ds", "gpt-4o")

	// ds2 is the rule's match_downstream
	addDownstream(t, s, "rule-ds", "RuleDS", ds2Server.URL, "key-2")

	// Rule references rule-ds in match_downstreams, but model resolves to model-ds
	// Rules no longer override the downstream - they only contribute pipelines
	addRule(t, s, "r1", "NoOverride", "/v1/chat/completions", "", "rule-ds", "[]", true)

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	// Model-resolved downstream (ds1) should be used, NOT the rule's downstream
	if !ds1Hit {
		t.Fatal("model-ds (ds1) should have been hit (output_model_ids resolution)")
	}
	if ds2Hit {
		t.Fatal("rule-ds (ds2) should NOT have been hit - rules no longer override downstreams")
	}
	if string(respBody) != `{"from":"ds1"}` {
		t.Fatalf("expected response from ds1, got: %s", string(respBody))
	}
}

// TestEngine_HandleProxy_AutoTranslate_OpenAI2Anthropic verifies that when an
// OpenAI-format request hits an Anthropic downstream, the engine auto-inserts
// the openai2anthropic translation plugin.
func TestEngine_HandleProxy_AutoTranslate_OpenAI2Anthropic(t *testing.T) {
	s := newTestStore(t)

	var translatedHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		translatedHeader = r.Header.Get("X-Auto-Translated")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	// Anthropic downstream (api_formats: ["anthropic"])
	addDownstream(t, s, "anth-ds", "AnthDS", dsServer.URL, "key-1", "anthropic")
	addOutputModelIDs(t, s, "anth-ds", "claude-sonnet")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Send OpenAI-format request to Anthropic downstream
	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if translatedHeader != "openai2anthropic" {
		t.Fatalf("expected X-Auto-Translated: openai2anthropic, got %q", translatedHeader)
	}
}

// TestEngine_HandleProxy_AutoTranslate_SameFormat verifies that when the input
// format is contained in the downstream's api_formats, no translation is applied.
func TestEngine_HandleProxy_AutoTranslate_SameFormat(t *testing.T) {
	s := newTestStore(t)

	var translatedHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		translatedHeader = r.Header.Get("X-Auto-Translated")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	// OpenAI downstream (api_formats: ["openai"])
	addDownstream(t, s, "oa-ds", "OADS", dsServer.URL, "key-1", "openai")
	addOutputModelIDs(t, s, "oa-ds", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Send OpenAI-format request to OpenAI downstream (same format)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if translatedHeader != "" {
		t.Fatalf("expected no auto-translation for same format, got X-Auto-Translated: %q", translatedHeader)
	}
}

// TestEngine_HandleProxy_AutoTranslate_NoTag verifies that a downstream without
// api_formats does not trigger auto-translation.
func TestEngine_HandleProxy_AutoTranslate_NoTag(t *testing.T) {
	s := newTestStore(t)

	var translatedHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		translatedHeader = r.Header.Get("X-Auto-Translated")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	// Downstream with no api_formats tag (empty array)
	addDownstream(t, s, "plain-ds", "PlainDS", dsServer.URL, "key-1")
	addOutputModelIDs(t, s, "plain-ds", "gpt-4o")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Send OpenAI-format request to downstream with no format tag
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if translatedHeader != "" {
		t.Fatalf("expected no auto-translation when downstream has no format tag, got X-Auto-Translated: %q", translatedHeader)
	}
}

// TestEngine_HandleProxy_AutoTranslate_Anthropic2OpenAI verifies that when an
// Anthropic-format request hits an OpenAI downstream, the engine auto-inserts
// the anthropic2openai translation plugin.
func TestEngine_HandleProxy_AutoTranslate_Anthropic2OpenAI(t *testing.T) {
	s := newTestStore(t)

	var translatedHeader string
	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		translatedHeader = r.Header.Get("X-Auto-Translated")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dsServer.Close()

	// OpenAI downstream (api_formats: ["openai"])
	addDownstream(t, s, "oa-ds", "OADS", dsServer.URL, "key-1", "openai")
	addOutputModelIDs(t, s, "oa-ds", "claude-sonnet")

	eng := New(s)
	eng.SetRegistry(&mockRegistryImpl{})

	// Send Anthropic-format request to OpenAI downstream
	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	eng.HandleProxy(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if translatedHeader != "anthropic2openai" {
		t.Fatalf("expected X-Auto-Translated: anthropic2openai, got %q", translatedHeader)
	}
}
