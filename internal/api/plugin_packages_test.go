package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/jsplugins"
	"tresor/internal/middleware"
	"tresor/internal/plugins"
)

func TestJSPipelineConfigurationValidatedOnSave(t *testing.T) {
	r := newTestRouter(t)
	defer r.Stop()
	manager, err := jsplugins.NewManager(t.TempDir(), plugins.BuiltinIDs())
	if err != nil {
		t.Fatal(err)
	}
	r.SetPluginManager(manager)
	files := map[string]string{"manifest.json": `{"id":"test/config","version":"1.0.0","api_version":2,"entry":"plugin.js","config_schema":{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}}`, "plugin.js": `module.exports={request(){}}`}
	if _, err := manager.Install(files); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate("test/config", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, pipeline := range []string{`[{"plugin_id":"test/config"}]`, `[{"plugin_id":"test/missing"}]`, `[{"plugin_id":"test/config","config":{"value":3}}}]`} {
		body, _ := json.Marshal(map[string]any{"name": "Invalid", "pattern_path": "*", "pipeline_config": pipeline})
		req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("invalid config saved: %d %s", w.Code, w.Body)
		}
	}
	if err := r.validateJSPipeline(`[{"plugin_id":"test/config","config":{"value":"valid"}}]`); err != nil {
		t.Fatal(err)
	}
}

func TestPluginPackagesInstallActivateDiscovery(t *testing.T) {
	r := newTestRouter(t)
	defer r.Stop()
	manager, err := jsplugins.NewManager(t.TempDir(), plugins.BuiltinIDs())
	if err != nil {
		t.Fatal(err)
	}
	r.SetPluginManager(manager)
	r.engine.SetRegistry(plugins.NewManagedRegistry(manager))
	handler := r.Handler()
	request := func(method, path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(value)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	manifest := `{"id":"test/headers","version":"1.0.0","description":"test local package","api_version":2,"entry":"plugin.js","requires":[],"permissions":[],"config_schema":{"type":"object"}}`
	files := map[string]string{"manifest.json": manifest, "plugin.js": `module.exports = { request({ request }) { return { request: { setHeaders: { 'x-local': ['yes'] } } }; } };`}
	w := request(http.MethodPost, "/api/plugin-packages/install", map[string]any{"files": files})
	if w.Code != 201 {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	w = request(http.MethodGet, "/api/plugins", nil)
	if bytes.Contains(w.Body.Bytes(), []byte(`test/headers`)) {
		t.Fatal("install activated package implicitly")
	}
	w = request(http.MethodPost, "/api/plugin-packages/activate", map[string]string{"id": "test/headers", "version": "1.0.0"})
	if w.Code != 200 {
		t.Fatalf("activate: %d %s", w.Code, w.Body)
	}
	w = request(http.MethodGet, "/api/plugins", nil)
	if !bytes.Contains(w.Body.Bytes(), []byte(`test/headers`)) {
		t.Fatalf("missing active plugin: %s", w.Body)
	}
	w = request(http.MethodPost, "/api/plugin-packages/activate", map[string]string{"id": "test/headers", "version": "missing"})
	if w.Code == 200 {
		t.Fatal("invalid activation accepted")
	}
	w = request(http.MethodGet, "/api/plugins", nil)
	if !bytes.Contains(w.Body.Bytes(), []byte(`test/headers`)) {
		t.Fatal("failed activation changed discovery")
	}
	w = request(http.MethodGet, "/api/plugin-packages", nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`test/headers`)) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

func TestURLOnlyRemoteRequestReachesTransport(t *testing.T) {
	r := newTestRouter(t)
	defer r.Stop()
	manager, err := jsplugins.NewManager(t.TempDir(), plugins.BuiltinIDs())
	if err != nil {
		t.Fatal(err)
	}
	r.SetPluginManager(manager)
	// A public-looking URL resolving to loopback is rejected by transport, not
	// by an absent SHA or the API decoder. This test never contacts the network.
	req := httptest.NewRequest(http.MethodPost, "/api/plugin-packages/install-remote", strings.NewReader(`{"url":"https://127.0.0.1/package"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "non-public package destination prohibited") {
		t.Fatalf("URL-only input not accepted: %d %s", w.Code, w.Body)
	}
}

func TestPluginPackageRoutesProtectedAndStrict(t *testing.T) {
	r := newTestRouter(t)
	defer r.Stop()
	old := r.authMW
	r.authMW = middleware.NewAuthMiddleware("test-secret")
	old.Stop()
	manager, err := jsplugins.NewManager(t.TempDir(), plugins.BuiltinIDs())
	if err != nil {
		t.Fatal(err)
	}
	r.SetPluginManager(manager)
	handler := r.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/plugin-packages", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("unauthenticated package route: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/plugin-packages/activate", bytes.NewBufferString(`{"id":"x/y","version":"1","extra":true}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("unknown input accepted: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/plugin-packages/install", bytes.NewBufferString(`{"files":{"../plugin.js":"bad","manifest.json":"{}"}}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("unsafe package accepted: %d", w.Code)
	}
	for _, path := range []string{"/api/plugin-packages/install", "/api/plugin-packages/activate", "/api/plugin-packages/install-remote", "/api/plugin-packages/approve", "/api/plugin-packages/revoke"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unprotected mutation %s: %d", path, w.Code)
		}
	}
	for _, test := range []struct {
		body, origin, site string
		want               int
	}{
		{body: `{"id":"x","id":"y","version":"1"}`, want: 400},
		{body: `{}`, origin: "https://other.example", want: 403},
		{body: `{}`, site: "cross-site", want: 403},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/plugin-packages/activate", strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Sec-Fetch-Site", test.site)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != test.want {
			t.Fatalf("strict mutation: %d %s", w.Code, w.Body)
		}
	}
	req = httptest.NewRequest(http.MethodPost, "/api/plugin-packages/install", strings.NewReader(`{"files":{"plugin.js":"`+strings.Repeat("x", maxPackageUpload)+`"}}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/plugin-packages/activate", strings.NewReader(`{"id":"x/y","version":"1"} {}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multiple JSON values accepted: %d", w.Code)
	}
}
