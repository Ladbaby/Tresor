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
	"tresor/internal/proxy"
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
		if err := proxy.ValidateOutboundURL(ds.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid base_url: "+err.Error())
			return
		}
		if err := r.store.CreateDownstream(&ds); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
		ds.APIKey = "***"
		writeJSONWithWarning(w, http.StatusCreated, ds, proxy.IsBareIP(ds.BaseURL))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDownstreamByID handles GET, PUT, DELETE on /api/downstreams/{id}
// and sub-resource operations on /api/downstreams/{id}/models,
// /api/downstreams/{id}/models/{model_id}, and /api/downstreams/{id}/fetch-models.
func (r *Router) handleDownstreamByID(w http.ResponseWriter, req *http.Request) {
	suffix := strings.TrimPrefix(req.URL.Path, "/api/downstreams/")

	// Parse suffix into segments: {id}[/models[/{model_id}]] or {id}[/fetch-models]
	parts := strings.SplitN(suffix, "/", 3)

	// Need at least the ID segment
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, req)
		return
	}

	id := parts[0]
	subResource := ""
	modelID := ""
	if len(parts) >= 2 {
		subResource = parts[1]
	}
	if len(parts) >= 3 {
		modelID = parts[2]
	}

	switch {
	case subResource == "models" && modelID == "":
		// POST /api/downstreams/{id}/models (add a model)
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleDownstreamModels(w, req, id, "")
	case subResource == "models" && modelID != "":
		// DELETE /api/downstreams/{id}/models/{model_id}
		if req.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleDownstreamModels(w, req, id, modelID)
	case subResource == "fetch-models":
		// POST /api/downstreams/{id}/fetch-models
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleDownstreamFetchModels(w, req, id)
	case subResource == "":
		// Direct downstream operations: /{id}
		r.handleDownstreamByIDDirect(w, req, id)
	default:
		http.NotFound(w, req)
	}
}

// handleDownstreamByIDDirect handles GET, PUT, DELETE on /api/downstreams/{id}.
func (r *Router) handleDownstreamByIDDirect(w http.ResponseWriter, req *http.Request, id string) {
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

		// Validate base_url if being updated
		if ds.BaseURL != "" {
			if err := proxy.ValidateOutboundURL(ds.BaseURL); err != nil {
				writeError(w, http.StatusBadRequest, "invalid base_url: "+err.Error())
				return
			}
		}

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
		_ = r.writeConfig()
		ds.APIKey = "***"
		writeJSONWithWarning(w, http.StatusOK, ds, proxy.IsBareIP(ds.BaseURL))

	case http.MethodDelete:
		if err := r.store.DeleteDownstream(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDownstreamModels handles POST /api/downstreams/{id}/models (add) and
// DELETE /api/downstreams/{id}/models/{model_id} (remove).
func (r *Router) handleDownstreamModels(w http.ResponseWriter, req *http.Request, id, modelID string) {
	// Validate downstream exists
	ds, err := r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if modelID == "" {
		// POST — Add a single model ID
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
	} else {
		// DELETE — Remove a single model ID
		if err := r.store.RemoveOutputModelID(id, modelID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	ds, err = r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds.APIKey != "" {
		ds.APIKey = "***"
	}
	_ = r.writeConfig()
	writeJSON(w, http.StatusOK, ds)
}

// handleDownstreamFetchModels handles POST /api/downstreams/{id}/fetch-models.
func (r *Router) handleDownstreamFetchModels(w http.ResponseWriter, req *http.Request, id string) {
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
	_ = r.writeConfig()
	writeJSON(w, http.StatusOK, ds)
}

// fetchModels calls the downstream provider's /models endpoint to discover available models.
// It tries multiple URL patterns (/v1/models, /models) and returns specific error messages.
func (r *Router) fetchModels(ds *store.Downstream) ([]string, error) {
	return fetchModelsByCreds(ds.BaseURL, ds.APIKey)
}

// fetchModelsByCreds fetches models given raw credentials (used for both existing
// downstreams and the create-new-downstream form).
func fetchModelsByCreds(baseURL, apiKey string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key configured — add an API key before fetching models")
	}

	baseURL = strings.TrimSuffix(baseURL, "/")

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
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastError = "unable to connect to provider"
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("authentication failed — check the API key")
		}
		if resp.StatusCode >= 400 {
			lastError = "provider returned unexpected response"
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

		lastError = "unrecognized response format"
	}

	return nil, fmt.Errorf("%s. No working models endpoint found", lastError)
}

// handleFetchModels handles POST /api/fetch-models.
// Accepts a JSON body with base_url and api_key (no downstream ID required),
// allowing the create-provider form to fetch models before saving.
func (r *Router) handleFetchModels(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.BaseURL == "" {
		writeError(w, http.StatusBadRequest, "base_url is required")
		return
	}
	if err := proxy.ValidateOutboundURL(body.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid base_url: "+err.Error())
		return
	}

	models, err := fetchModelsByCreds(body.BaseURL, body.APIKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "fetch failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string][]string{"model_ids": models})
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
