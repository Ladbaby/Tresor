package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"tresor/internal/store"
)

// createDownstreamViaAPI creates a downstream via the POST /api/downstreams endpoint.
func createDownstreamViaAPI(t *testing.T, handler http.Handler, name, baseURL string) store.Downstream {
	t.Helper()
	body := map[string]interface{}{
		"name":     name,
		"base_url": baseURL,
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating downstream, got %d: %s", w.Code, w.Body.String())
	}
	var ds store.Downstream
	if err := json.NewDecoder(w.Body).Decode(&ds); err != nil {
		t.Fatalf("decode downstream response: %v", err)
	}
	return ds
}

func TestListDownstreams_Empty(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/downstreams", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var ds []store.Downstream
	if err := json.NewDecoder(w.Body).Decode(&ds); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(ds) != 0 {
		t.Fatalf("expected empty list, got %d downstreams", len(ds))
	}
}

func TestCreateDownstream_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name":     "test-provider",
		"base_url": "https://api.test.com",
		"api_key":  "sk-test-key",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var ds store.Downstream
	json.NewDecoder(w.Body).Decode(&ds)
	if ds.Name != "test-provider" {
		t.Fatalf("expected name 'test-provider', got %q", ds.Name)
	}
	if ds.APIKey != "***" {
		t.Fatalf("expected masked API key, got %q", ds.APIKey)
	}
}

func TestCreateDownstream_MissingName(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"base_url": "https://api.test.com",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateDownstream_MissingBaseURL(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name": "test-provider",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing base_url, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetDownstream_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "get-test", "https://get.test.com")

	req := httptest.NewRequest(http.MethodGet, "/api/downstreams/"+ds.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got store.Downstream
	json.NewDecoder(w.Body).Decode(&got)
	if got.ID != ds.ID {
		t.Fatalf("expected id %s, got %s", ds.ID, got.ID)
	}
}

func TestGetDownstream_NotFound(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/downstreams/nonexistent-id", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateDownstream(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "old-name", "https://old.test.com")

	body := map[string]interface{}{
		"name":     "new-name",
		"base_url": "https://new.test.com",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/downstreams/"+ds.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated store.Downstream
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Name != "new-name" {
		t.Fatalf("expected name 'new-name', got %q", updated.Name)
	}
}

func TestDeleteDownstream(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "delete-me", "https://del.test.com")

	req := httptest.NewRequest(http.MethodDelete, "/api/downstreams/"+ds.ID, nil)
	w := httptest.NewRecorder()
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
	req = httptest.NewRequest(http.MethodGet, "/api/downstreams/"+ds.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", w.Code)
	}
}

func TestListDownstreams_MasksAPIKeys(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name":     "key-test",
		"base_url": "https://key.test.com",
		"api_key":  "sk-real-secret-key",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create downstream: expected 201, got %d", w.Code)
	}

	// List and check masking
	req = httptest.NewRequest(http.MethodGet, "/api/downstreams", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var dsList []store.Downstream
	json.NewDecoder(w.Body).Decode(&dsList)
	for _, d := range dsList {
		if d.APIKey == "sk-real-secret-key" {
			t.Fatal("API key was not masked in list response")
		}
	}
}

func TestAddRemoveModel(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "model-test", "https://model.test.com")

	// Add a model
	addBody := map[string]interface{}{"model_id": "gpt-4o-mini"}
	data, _ := json.Marshal(addBody)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams/"+ds.ID+"/models", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("add model: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated store.Downstream
	json.NewDecoder(w.Body).Decode(&updated)
	if len(updated.OutputModelIDs) != 1 || updated.OutputModelIDs[0] != "gpt-4o-mini" {
		t.Fatalf("expected [gpt-4o-mini], got %v", updated.OutputModelIDs)
	}

	// Remove the model
	deletePath := "/api/downstreams/" + ds.ID + "/models/gpt-4o-mini"
	req = httptest.NewRequest(http.MethodDelete, deletePath, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("remove model: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var afterDelete store.Downstream
	json.NewDecoder(w.Body).Decode(&afterDelete)
	// After removal, output_model_ids is omitted from JSON (omitempty on empty slice).
	// Verify by checking the raw body doesn't contain the model id.
	if bytes.Contains(w.Body.Bytes(), []byte("gpt-4o-mini")) {
		t.Fatalf("expected model to be removed, but it's still in response: %s", w.Body.String())
	}
	if len(afterDelete.OutputModelIDs) > 0 && afterDelete.OutputModelIDs[0] == "gpt-4o-mini" {
		// If the field is empty (omitted by JSON omitempty), json.Decode won't clear it.
		// So check the raw response body instead.
		var raw map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &raw)
		if ids, ok := raw["output_model_ids"].([]interface{}); ok && len(ids) > 0 {
			t.Fatalf("expected 0 models after removal, got %d", len(ids))
		}
	}
}

func TestAddModel_EmptyModelID(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := createDownstreamViaAPI(t, handler, "empty-model-test", "https://empty.test.com")

	body := map[string]interface{}{"model_id": ""}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams/"+ds.ID+"/models", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty model_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDownstreamModels_NotFound(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{"model_id": "gpt-4o"}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams/nonexistent-ds/models", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent downstream, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Partial-update regression tests for PUT /api/downstreams/{id} ---

// seedDownstreamForPatch creates a downstream, sets its api_formats via PUT, and
// adds the given output_model_ids via POST /models. Returns the full seeded
// record as a fresh GetDownstream read.
func seedDownstreamForPatch(t *testing.T, handler http.Handler, name, baseURL string, formats []string, models []string) store.Downstream {
	t.Helper()
	ds := createDownstreamViaAPI(t, handler, name, baseURL)

	if len(formats) > 0 {
		body, _ := json.Marshal(map[string]interface{}{"api_formats": formats})
		req := httptest.NewRequest(http.MethodPut, "/api/downstreams/"+ds.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("seed formats: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	for _, m := range models {
		body, _ := json.Marshal(map[string]interface{}{"model_id": m})
		req := httptest.NewRequest(http.MethodPost, "/api/downstreams/"+ds.ID+"/models", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("seed model %q: expected 200, got %d: %s", m, w.Code, w.Body.String())
		}
	}

	got, err := getDownstreamFresh(handler, ds.ID)
	if err != nil {
		t.Fatalf("re-read seeded downstream: %v", err)
	}
	return got
}

// getDownstreamFresh fetches a downstream via GET (returns a *Downstream from the
// store). We can't reuse the handler response's masking reliably, so go direct.
func getDownstreamFresh(handler http.Handler, id string) (store.Downstream, error) {
	req := httptest.NewRequest(http.MethodGet, "/api/downstreams/"+id, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return store.Downstream{}, fmt.Errorf("get downstream %s: status %d: %s", id, w.Code, w.Body.String())
	}
	var ds store.Downstream
	if err := json.NewDecoder(w.Body).Decode(&ds); err != nil {
		return store.Downstream{}, err
	}
	return ds, nil
}

// patchDownstream sends a PUT with the given body and returns the status code.
func patchDownstream(handler http.Handler, id string, body interface{}) int {
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/downstreams/"+id, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

// sortedCopy returns a sorted copy of s so test assertions don't depend on the
// store's internal ordering (output_model_ids is returned ORDER BY model_id,
// which is sorted, but be defensive).
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// Reported bug: toggling a single API format checkbox wiped name, base_url,
// and output_model_ids because the handler did a full-replace UPDATE.
func TestUpdateDownstream_PartialPatch_PreservesOtherFields(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	seeded := seedDownstreamForPatch(t, handler, "name-x", "https://x.test",
		[]string{"openai", "anthropic"}, []string{"gpt-4o-mini", "gpt-4o"})

	if got := patchDownstream(handler, seeded.ID, map[string]interface{}{
		"api_formats": []string{"openai"},
	}); got != http.StatusOK {
		t.Fatalf("expected 200, got %d", got)
	}

	after, err := getDownstreamFresh(handler, seeded.ID)
	if err != nil {
		t.Fatalf("read after patch: %v", err)
	}
	if after.Name != "name-x" {
		t.Fatalf("name wiped: got %q", after.Name)
	}
	if after.BaseURL != "https://x.test" {
		t.Fatalf("base_url wiped: got %q", after.BaseURL)
	}
	if !reflect.DeepEqual(sortedCopy(after.OutputModelIDs), []string{"gpt-4o", "gpt-4o-mini"}) {
		t.Fatalf("output_model_ids wiped: got %v", after.OutputModelIDs)
	}
	if !reflect.DeepEqual(after.ApiFormats, []string{"openai"}) {
		t.Fatalf("api_formats not replaced as expected: got %v", after.ApiFormats)
	}
}

// api_formats: [] must mean "clear it", not "no change". Pointer fields let us
// distinguish absent (nil) from explicit-empty (non-nil pointer to []string{}).
func TestUpdateDownstream_ExplicitEmptyApiFormats_Clears(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	seeded := seedDownstreamForPatch(t, handler, "fmt-clear", "https://fc.test",
		[]string{"openai", "anthropic"}, []string{"gpt-4o"})

	if got := patchDownstream(handler, seeded.ID, map[string]interface{}{
		"api_formats": []string{},
	}); got != http.StatusOK {
		t.Fatalf("expected 200, got %d", got)
	}

	after, err := getDownstreamFresh(handler, seeded.ID)
	if err != nil {
		t.Fatalf("read after patch: %v", err)
	}
	if len(after.ApiFormats) != 0 {
		t.Fatalf("api_formats should be cleared, got %v", after.ApiFormats)
	}
	// Other fields must survive the explicit-empty patch.
	if after.Name != "fmt-clear" {
		t.Fatalf("name wiped: got %q", after.Name)
	}
	if after.BaseURL != "https://fc.test" {
		t.Fatalf("base_url wiped: got %q", after.BaseURL)
	}
	if !reflect.DeepEqual(sortedCopy(after.OutputModelIDs), []string{"gpt-4o"}) {
		t.Fatalf("output_model_ids wiped: got %v", after.OutputModelIDs)
	}
}

// api_key: "***" is the masked placeholder the web UI never sends in a patch
// (it sends nothing or a real replacement). It must mean "do not change".
func TestUpdateDownstream_MaskedAPIKey_Preserves(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	body := map[string]interface{}{
		"name":     "key-preserve",
		"base_url": "https://kp.test",
		"api_key":  "sk-real-secret",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var ds store.Downstream
	json.NewDecoder(w.Body).Decode(&ds)

	// Web UI semantics: it sends {api_key: "***"} when the user didn't type
	// a new value but the field was included in the patch body.
	if got := patchDownstream(handler, ds.ID, map[string]interface{}{
		"api_key": "***",
	}); got != http.StatusOK {
		t.Fatalf("expected 200, got %d", got)
	}

	// Use reveal=1 to read the real stored key.
	revealReq := httptest.NewRequest(http.MethodGet, "/api/downstreams/"+ds.ID+"?reveal=1", nil)
	revealW := httptest.NewRecorder()
	handler.ServeHTTP(revealW, revealReq)
	if revealW.Code != http.StatusOK {
		t.Fatalf("reveal: expected 200, got %d", revealW.Code)
	}
	var revealed store.Downstream
	json.NewDecoder(revealW.Body).Decode(&revealed)
	if revealed.APIKey != "sk-real-secret" {
		t.Fatalf("api_key changed despite *** placeholder, got %q", revealed.APIKey)
	}
}

// output_model_ids: [] must clear models while preserving formats and other fields.
func TestUpdateDownstream_ExplicitEmptyModels_Clears(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	seeded := seedDownstreamForPatch(t, handler, "models-clear", "https://mc.test",
		[]string{"openai"}, []string{"gpt-4o", "gpt-4o-mini"})

	if got := patchDownstream(handler, seeded.ID, map[string]interface{}{
		"output_model_ids": []string{},
	}); got != http.StatusOK {
		t.Fatalf("expected 200, got %d", got)
	}

	after, err := getDownstreamFresh(handler, seeded.ID)
	if err != nil {
		t.Fatalf("read after patch: %v", err)
	}
	if len(after.OutputModelIDs) != 0 {
		t.Fatalf("output_model_ids should be cleared, got %v", after.OutputModelIDs)
	}
	if !reflect.DeepEqual(after.ApiFormats, []string{"openai"}) {
		t.Fatalf("api_formats wiped: got %v", after.ApiFormats)
	}
	if after.Name != "models-clear" {
		t.Fatalf("name wiped: got %q", after.Name)
	}
	if after.BaseURL != "https://mc.test" {
		t.Fatalf("base_url wiped: got %q", after.BaseURL)
	}
}
