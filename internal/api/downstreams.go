package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tresor/internal/engine"
	"tresor/internal/store"
)

// handleDownstreams handles GET and POST on /api/downstreams.
func (r *Router) handleDownstreams(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		downstreams, err := r.store.ListDownstreams()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if downstreams == nil {
			downstreams = []store.Downstream{}
		}
		// Mask API keys in responses
		for i := range downstreams {
			if downstreams[i].APIKey != "" {
				downstreams[i].APIKey = "***"
			}
		}
		writeJSON(w, http.StatusOK, downstreams)

	case http.MethodPost:
		var ds store.Downstream
		if err := json.NewDecoder(req.Body).Decode(&ds); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if ds.Name == "" || ds.BaseURL == "" {
			writeError(w, http.StatusBadRequest, "name and base_url are required")
			return
		}
		if err := r.store.CreateDownstream(&ds); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.writeConfig()
		ds.APIKey = "***"
		writeJSON(w, http.StatusCreated, ds)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDownstreamByID handles GET, PUT, DELETE on /api/downstreams/{id}
// and model operations (/models, /fetch-models).
func (r *Router) handleDownstreamByID(w http.ResponseWriter, req *http.Request) {
	suffix := strings.TrimPrefix(req.URL.Path, "/api/downstreams/")

	// --- Sub-resource paths: /{id}/models or /{id}/fetch-models ---
	if strings.Contains(suffix, "/models") {
		r.handleDownstreamModels(w, req)
		return
	}
	if strings.Contains(suffix, "/fetch-models") {
		r.handleDownstreamFetchModels(w, req)
		return
	}

	// --- Direct downstream operations: /{id} ---
	id := suffix

	switch req.Method {
	case http.MethodGet:
		ds, err := r.store.GetDownstream(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if ds.APIKey != "" {
			ds.APIKey = "***"
		}
		writeJSON(w, http.StatusOK, ds)

	case http.MethodPut:
		var ds store.Downstream
		if err := json.NewDecoder(req.Body).Decode(&ds); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		ds.ID = id

		// Preserve the existing API key if the client sent nothing or the masked placeholder.
		if ds.APIKey == "" || ds.APIKey == "***" {
			existing, err := r.store.GetDownstream(id)
			if err == nil {
				ds.APIKey = existing.APIKey
			}
		}
		if err := r.store.UpdateDownstream(&ds); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.writeConfig()
		ds.APIKey = "***"
		writeJSON(w, http.StatusOK, ds)

	case http.MethodDelete:
		if err := r.store.DeleteDownstream(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.writeConfig()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDownstreamModels handles POST /api/downstreams/{id}/models (add) and
// DELETE /api/downstreams/{id}/models/{model_id} (remove).
func (r *Router) handleDownstreamModels(w http.ResponseWriter, req *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/api/downstreams/"), "/", 3)
	id := parts[0]

	// Validate downstream exists
	ds, err := r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	switch req.Method {
	case http.MethodPost:
		// Add a single model ID: POST /api/downstreams/{id}/models {"model_id": "gpt-4o-mini"}
		var body struct {
			ModelID string `json:"model_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.ModelID == "" {
			writeError(w, http.StatusBadRequest, "model_id is required")
			return
		}
		if err := r.store.AddOutputModelID(id, body.ModelID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ds, err = r.store.GetDownstream(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

	case http.MethodDelete:
		// Remove a single model ID: DELETE /api/downstreams/{id}/models/{model_id}
		if len(parts) < 3 || parts[2] == "" {
			writeError(w, http.StatusBadRequest, "model_id is required in path")
			return
		}
		modelID := parts[2]
		if err := r.store.RemoveOutputModelID(id, modelID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ds, err = r.store.GetDownstream(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if ds.APIKey != "" {
		ds.APIKey = "***"
	}
	r.writeConfig()
	writeJSON(w, http.StatusOK, ds)
}

// handleDownstreamFetchModels handles POST /api/downstreams/{id}/fetch-models.
func (r *Router) handleDownstreamFetchModels(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/api/downstreams/"), "/", 2)
	id := parts[0]

	ds, err := r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	models, fetchErr := r.fetchModels(ds)
	if fetchErr != nil {
		writeError(w, http.StatusBadRequest, "fetch failed: "+fetchErr.Error())
		return
	}

	for _, m := range models {
		if err := r.store.AddOutputModelID(id, m); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Reload to return updated downstream
	ds, err = r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds.APIKey != "" {
		ds.APIKey = "***"
	}
	r.writeConfig()
	writeJSON(w, http.StatusOK, ds)
}

// fetchModels calls the downstream provider's /models endpoint to discover available models.
// It tries multiple URL patterns (/v1/models, /models) and returns specific error messages.
func (r *Router) fetchModels(ds *store.Downstream) ([]string, error) {
	if ds.APIKey == "" {
		return nil, fmt.Errorf("no API key configured for downstream \"%s\" — add an API key before fetching models", ds.Name)
	}

	baseURL := strings.TrimSuffix(ds.BaseURL, "/")

	// Try common model endpoint patterns
	endpoints := []string{
		baseURL + "/models",   // OpenAI-style /v1/models (base_url already includes /v1)
		baseURL + "/v1/models", // Some providers need explicit /v1 prefix
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastError string

	for _, url := range endpoints {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+ds.APIKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastError = fmt.Sprintf("could not reach %s — check the base_url and network connectivity", url)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("authentication failed (%d) for downstream \"%s\" at %s — check the API key", resp.StatusCode, ds.Name, url)
		}
		if resp.StatusCode >= 400 {
			lastError = fmt.Sprintf("request to %s returned HTTP %d", url, resp.StatusCode)
			continue
		}

		// Try to parse as OpenAI-style response: {"data": [{"id": "..."}]}
		var openaiResp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &openaiResp); err == nil && len(openaiResp.Data) > 0 {
			models := make([]string, 0, len(openaiResp.Data))
			for _, m := range openaiResp.Data {
				models = append(models, m.ID)
			}
			return models, nil
		}

		// Try Anthropic-style: {"data": [{"id": "..."}]}
		// (same structure, so already handled above)

		// Fallback: try raw array of strings
		var strArr []string
		if err := json.Unmarshal(body, &strArr); err == nil && len(strArr) > 0 {
			return strArr, nil
		}

		lastError = fmt.Sprintf("unrecognized response format from %s", url)
	}

	return nil, fmt.Errorf("%s. No working models endpoint found among: %v", lastError, endpoints)
}

// handlePlugins returns the list of registered plugins.
func (r *Router) handlePlugins(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	plugins := r.engine.Registry().ListPlugins()
	if plugins == nil {
		plugins = []engine.PluginInfo{}
	}
	writeJSON(w, http.StatusOK, plugins)
}
