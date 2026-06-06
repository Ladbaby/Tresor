package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"base_url": "https://api.test.com/v1",
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
		"base_url": "https://api.test.com/v1",
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

	ds := createDownstreamViaAPI(t, handler, "get-test", "https://get.test.com/v1")

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

	ds := createDownstreamViaAPI(t, handler, "old-name", "https://old.test.com/v1")

	body := map[string]interface{}{
		"name":     "new-name",
		"base_url": "https://new.test.com/v1",
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

	ds := createDownstreamViaAPI(t, handler, "delete-me", "https://del.test.com/v1")

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
		"base_url": "https://key.test.com/v1",
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

	ds := createDownstreamViaAPI(t, handler, "model-test", "https://model.test.com/v1")

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

	ds := createDownstreamViaAPI(t, handler, "empty-model-test", "https://empty.test.com/v1")

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
