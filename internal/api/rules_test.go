package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"tresor/internal/config"
	"tresor/internal/engine"
	"tresor/internal/store"
)

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	f, err := os.CreateTemp("", "tresor-api-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := store.Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := &config.AppConfig{DBPath: f.Name()}
	eng := engine.New(s)
	logger := engine.NewRequestLogger()
	return NewRouter(s, eng, logger, cfg, "test", "unknown")
}

// --- New extended tests below ---

func TestListRules_Empty(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rules []store.Rule
	if err := json.NewDecoder(w.Body).Decode(&rules); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected empty list, got %d rules", len(rules))
	}
}

func TestCreateRule_MissingName(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"pattern_path": "/v1/chat/completions",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRule_MissingPatternPath(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name": "test-rule",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing pattern_path, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRule_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "rule-ds", "https://rule.test.com/v1")

	body := map[string]interface{}{
		"name":              "test-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var rule store.Rule
	json.NewDecoder(w.Body).Decode(&rule)
	if rule.Name != "test-rule" {
		t.Fatalf("expected name 'test-rule', got %q", rule.Name)
	}
}

func TestGetRule_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "get-rule-ds", "https://getrule.test.com/v1")

	ruleBody := map[string]interface{}{
		"name":              "get-test-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(ruleBody)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d", w.Code)
	}
	var created store.Rule
	json.NewDecoder(w.Body).Decode(&created)

	req = httptest.NewRequest(http.MethodGet, "/api/rules/"+created.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got store.Rule
	json.NewDecoder(w.Body).Decode(&got)
	if got.ID != created.ID {
		t.Fatalf("expected id %s, got %s", created.ID, got.ID)
	}
}

func TestGetRule_NotFound(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/rules/nonexistent-id", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateRule_FullUpdate(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "update-rule-ds", "https://updaterule.test.com/v1")

	ruleBody := map[string]interface{}{
		"name":              "old-name",
		"pattern_path":      "/old/path",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(ruleBody)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d", w.Code)
	}
	var created store.Rule
	json.NewDecoder(w.Body).Decode(&created)

	updateBody := map[string]interface{}{
		"name":         "new-name",
		"pattern_path": "/new/path",
	}
	data, _ = json.Marshal(updateBody)
	req = httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated store.Rule
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Name != "new-name" {
		t.Fatalf("expected name 'new-name', got %q", updated.Name)
	}
	if updated.PatternPath != "/new/path" {
		t.Fatalf("expected pattern_path '/new/path', got %q", updated.PatternPath)
	}
}

func TestUpdateRule_EnabledOnly(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "enable-rule-ds", "https://enablerule.test.com/v1")

	ruleBody := map[string]interface{}{
		"name":              "enable-test-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(ruleBody)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d", w.Code)
	}
	var created store.Rule
	json.NewDecoder(w.Body).Decode(&created)

	updateBody := map[string]interface{}{
		"is_enabled": false,
	}
	data, _ = json.Marshal(updateBody)
	req = httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated store.Rule
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.IsEnabled {
		t.Fatal("expected rule to be disabled")
	}
}

func TestUpdateRule_NoFields(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "nofields-ds", "https://nofields.test.com/v1")

	ruleBody := map[string]interface{}{
		"name":              "no-fields-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(ruleBody)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d", w.Code)
	}
	var created store.Rule
	json.NewDecoder(w.Body).Decode(&created)

	updateBody := map[string]interface{}{}
	data, _ = json.Marshal(updateBody)
	req = httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty update, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteRule(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "del-rule-ds", "https://delrule.test.com/v1")

	ruleBody := map[string]interface{}{
		"name":              "delete-me-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{ds.ID},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(ruleBody)
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d", w.Code)
	}
	var created store.Rule
	json.NewDecoder(w.Body).Decode(&created)

	req = httptest.NewRequest(http.MethodDelete, "/api/rules/"+created.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "deleted" {
		t.Fatalf("expected status 'deleted', got %q", resp["status"])
	}

	// Verify it's gone
	req = httptest.NewRequest(http.MethodGet, "/api/rules/"+created.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", w.Code)
	}
}

func TestCreateRule_RejectsInvalidDownstream(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name":              "bad-rule",
		"pattern_path":      "/v1/chat/completions",
		"match_downstreams": []string{"nonexistent-downstream"},
		"pipeline_config":   "[]",
		"is_enabled":        true,
	}
	data, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid match_downstreams, got %d: %s", w.Code, w.Body.String())
	}
}

