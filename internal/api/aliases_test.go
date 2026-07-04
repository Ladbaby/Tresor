package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"tresor/internal/store"
)

// setupDownstream creates a downstream via the API and returns it.
func setupDownstream(t *testing.T, handler http.Handler, name, baseURL string) store.Downstream {
	t.Helper()
	ds := map[string]interface{}{
		"name":     name,
		"base_url": baseURL,
		"api_key":  "key-" + name,
	}
	data, _ := json.Marshal(ds)
	req := httptest.NewRequest(http.MethodPost, "/api/downstreams", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create downstream %s: got %d: %s", name, w.Code, w.Body.String())
	}
	var created store.Downstream
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode downstream: %v", err)
	}
	return created
}

// createAliasViaAPI sends a POST /api/aliases request and returns (status, Alias, body_str).
func createAliasViaAPI(t *testing.T, handler http.Handler, alias store.Alias) (int, store.Alias, string) {
	t.Helper()
	data, _ := json.Marshal(alias)
	req := httptest.NewRequest(http.MethodPost, "/api/aliases", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var created store.Alias
	if w.Code == http.StatusCreated {
		json.NewDecoder(w.Body).Decode(&created)
	}
	return w.Code, created, w.Body.String()
}

// TestCreateAlias_Success verifies POST /api/aliases creates an alias with 201.
func TestCreateAlias_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	code, created, bodyStr := createAliasViaAPI(t, handler, store.Alias{
		InputModelID:  "gpt-4o",
		OutputModelID: "claude-sonnet",
		DownstreamID:  ds.ID,
		IsActive:      true,
	})

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, bodyStr)
	}
	if created.InputModelID != "gpt-4o" {
		t.Fatalf("expected input_model_id gpt-4o, got %q", created.InputModelID)
	}
	if created.OutputModelID != "claude-sonnet" {
		t.Fatalf("expected output_model_id claude-sonnet, got %q", created.OutputModelID)
	}
	if !created.IsActive {
		t.Fatal("expected alias to be active")
	}
}

// TestCreateAlias_RejectsMissingFields verifies POST with missing required fields returns 400.
func TestCreateAlias_RejectsMissingFields(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	// Missing output_model_id
	data, _ := json.Marshal(store.Alias{
		InputModelID: "gpt-4o",
		DownstreamID: ds.ID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/aliases", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing output_model_id, got %d: %s", w.Code, w.Body.String())
	}

	// Missing downstream_id
	data, _ = json.Marshal(store.Alias{
		InputModelID:  "gpt-4o",
		OutputModelID: "claude-sonnet",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/aliases", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing downstream_id, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateAlias_RejectsInvalidDownstream verifies POST with non-existent downstream returns 400.
func TestCreateAlias_RejectsInvalidDownstream(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	data, _ := json.Marshal(store.Alias{
		InputModelID:  "gpt-4o",
		OutputModelID: "claude-sonnet",
		DownstreamID:  "nonexistent-ds",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/aliases", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid downstream, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListAliases_Empty verifies GET /api/aliases returns an empty array when no aliases exist.
func TestListAliases_Empty(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/aliases", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var groups []store.AliasGroup
	if err := json.NewDecoder(w.Body).Decode(&groups); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(groups))
	}
}

// TestListAliases_WithGroups verifies GET /api/aliases returns grouped aliases enriched with downstream names.
func TestListAliases_WithGroups(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds1 := setupDownstream(t, handler, "Provider-A", "https://a.test.com")
	ds2 := setupDownstream(t, handler, "Provider-B", "https://b.test.com")

	// Create two aliases for gpt-4o group (a1 active, a2 inactive)
	code, _, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "gpt-4o", DownstreamID: ds1.ID, IsActive: true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create alias 1: got %d", code)
	}
	code, _, _ = createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "claude-sonnet", DownstreamID: ds2.ID, IsActive: false,
	})
	if code != http.StatusCreated {
		t.Fatalf("create alias 2: got %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/aliases", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var groups []store.AliasGroup
	if err := json.NewDecoder(w.Body).Decode(&groups); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	g := groups[0]
	if g.InputModelID != "gpt-4o" {
		t.Fatalf("expected group gpt-4o, got %q", g.InputModelID)
	}
	if len(g.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(g.Options))
	}

	// Check downstream names are enriched
	for i, opt := range g.Options {
		if opt.DownstreamName == "" {
			t.Fatalf("option %d has empty downstream_name", i)
		}
	}

	// Active ID should point to the first alias (the active one)
	if g.ActiveID == nil {
		t.Fatal("expected active_id to be set")
	}
}

// TestGetAlias_Success verifies GET /api/aliases/{id} returns the alias.
func TestGetAlias_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	_, created, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "claude-sonnet", DownstreamID: ds.ID, IsActive: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/aliases/"+created.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got store.Alias
	json.NewDecoder(w.Body).Decode(&got)
	if got.InputModelID != "gpt-4o" {
		t.Fatalf("expected input_model_id gpt-4o, got %q", got.InputModelID)
	}
}

// TestGetAlias_NotFound verifies GET for a non-existent alias returns 404.
func TestGetAlias_NotFound(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/aliases/nonexistent-id", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestUpdateAlias_Success verifies PUT /api/aliases/{id} updates the alias.
func TestUpdateAlias_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	_, created, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "gpt-4o", DownstreamID: ds.ID, IsActive: true,
	})

	// Update output_model_id
	updateBody := map[string]interface{}{
		"output_model_id": "claude-sonnet",
	}
	data, _ := json.Marshal(updateBody)
	req := httptest.NewRequest(http.MethodPut, "/api/aliases/"+created.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated store.Alias
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.OutputModelID != "claude-sonnet" {
		t.Fatalf("expected output_model_id claude-sonnet, got %q", updated.OutputModelID)
	}
}

// TestUpdateAlias_RejectsInvalidDownstream verifies PUT with non-existent downstream returns 400.
func TestUpdateAlias_RejectsInvalidDownstream(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	_, created, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "gpt-4o", DownstreamID: ds.ID,
	})

	updateBody := map[string]interface{}{
		"downstream_id": "nonexistent-ds",
	}
	data, _ := json.Marshal(updateBody)
	req := httptest.NewRequest(http.MethodPut, "/api/aliases/"+created.ID, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid downstream, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDeleteAlias_Success verifies DELETE /api/aliases/{id} removes the alias.
func TestDeleteAlias_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds := setupDownstream(t, handler, "test-provider", "https://api.test.com")

	_, created, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "claude-sonnet", DownstreamID: ds.ID,
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/aliases/"+created.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it's gone
	req2 := httptest.NewRequest(http.MethodGet, "/api/aliases/"+created.ID, nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestActivateAlias_HotSwitch verifies PUT /api/aliases/{id}/activate activates the alias
// and deactivates siblings in the same group.
func TestActivateAlias_HotSwitch(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds1 := setupDownstream(t, handler, "Provider-A", "https://a.test.com")
	ds2 := setupDownstream(t, handler, "Provider-B", "https://b.test.com")

	// Create active alias pointing to ds1
	_, a1, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "gpt-4o", DownstreamID: ds1.ID, IsActive: true,
	})

	// Create inactive alias pointing to ds2
	_, a2, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "claude-sonnet", DownstreamID: ds2.ID, IsActive: false,
	})

	// Activate a2 — should deactivate a1
	req := httptest.NewRequest(http.MethodPut, "/api/aliases/"+a2.ID+"/activate", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for activate, got %d: %s", w.Code, w.Body.String())
	}

	var activated store.Alias
	json.NewDecoder(w.Body).Decode(&activated)
	if !activated.IsActive {
		t.Fatal("expected activated alias to be active")
	}

	// Verify a1 is now inactive
	req2 := httptest.NewRequest(http.MethodGet, "/api/aliases/"+a1.ID, nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var got store.Alias
	json.NewDecoder(w2.Body).Decode(&got)
	if got.IsActive {
		t.Fatal("expected a1 to be deactivated after activating a2")
	}
}

// TestCreateAlias_AutoDeactivatesSibling verifies that creating an active alias
// auto-deactivates any existing active alias in the same group.
func TestCreateAlias_AutoDeactivatesSibling(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds1 := setupDownstream(t, handler, "Provider-A", "https://a.test.com")
	ds2 := setupDownstream(t, handler, "Provider-B", "https://b.test.com")

	// Create first alias as active
	_, a1, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "gpt-4o", DownstreamID: ds1.ID, IsActive: true,
	})

	// Create second alias for same group as active — should auto-deactivate a1
	code, _, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o", OutputModelID: "claude-sonnet", DownstreamID: ds2.ID, IsActive: true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create alias 2: got %d", code)
	}

	// Verify a1 is now inactive
	req := httptest.NewRequest(http.MethodGet, "/api/aliases/"+a1.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var got store.Alias
	json.NewDecoder(w.Body).Decode(&got)
	if got.IsActive {
		t.Fatal("expected a1 to be auto-deactivated when a2 was created as active")
	}
}

// TestAliases_MethodNotAllowed verifies that unsupported methods return 405.
func TestAliases_MethodNotAllowed(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	// DELETE on collection endpoint should be 405
	req := httptest.NewRequest(http.MethodDelete, "/api/aliases", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE on /api/aliases, got %d: %s", w.Code, w.Body.String())
	}

	// POST on individual endpoint should be 405
	req2 := httptest.NewRequest(http.MethodPost, "/api/aliases/some-id", bytes.NewReader([]byte("{}")))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST on /api/aliases/{id}, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestDeleteAliasGroup_Success verifies DELETE /api/aliases/group/{inputModelId} deletes all aliases in the group.
func TestDeleteAliasGroup_Success(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	ds1 := setupDownstream(t, handler, "Provider-A", "https://a.test.com")
	ds2 := setupDownstream(t, handler, "Provider-B", "https://b.test.com")

	// Create 3 aliases for gpt-4o group
	for i, out := range []string{"model-a", "model-b", "model-c"} {
		code, _, _ := createAliasViaAPI(t, handler, store.Alias{
			InputModelID: "gpt-4o", OutputModelID: out, DownstreamID: ds1.ID, IsActive: i == 0,
		})
		if code != http.StatusCreated {
			t.Fatalf("create alias %s: got %d", out, code)
		}
	}

	// Create an alias for a different group (should not be deleted)
	code, _, _ := createAliasViaAPI(t, handler, store.Alias{
		InputModelID: "gpt-4o-mini", OutputModelID: "claude-haiku", DownstreamID: ds2.ID, IsActive: true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create gpt-4o-mini alias: got %d", code)
	}

	// Delete the gpt-4o group
	req := httptest.NewRequest(http.MethodDelete, "/api/aliases/group/gpt-4o", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delete group, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if int(resp["count"].(float64)) != 3 {
		t.Fatalf("expected count 3, got %v", resp["count"])
	}

	// Verify gpt-4o-mini alias still exists
	req2 := httptest.NewRequest(http.MethodGet, "/api/aliases", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var groups []store.AliasGroup
	json.NewDecoder(w2.Body).Decode(&groups)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group remaining, got %d", len(groups))
	}
	if groups[0].InputModelID != "gpt-4o-mini" {
		t.Fatalf("expected remaining group to be gpt-4o-mini, got %q", groups[0].InputModelID)
	}
}

// TestDeleteAliasGroup_NotFound verifies DELETE for a non-existent group returns 404.
func TestDeleteAliasGroup_NotFound(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodDelete, "/api/aliases/group/nonexistent-model", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent group, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDeleteAliasGroup_MethodNotAllowed verifies non-DELETE methods return 405.
func TestDeleteAliasGroup_MethodNotAllowed(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/aliases/group/gpt-4o", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET on group endpoint, got %d: %s", w.Code, w.Body.String())
	}
}
