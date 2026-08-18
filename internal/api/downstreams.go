package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
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
		// Validate per-format URL overrides, if any
		if ds.FormatURLs != nil {
			for f, u := range ds.FormatURLs {
				if err := proxy.ValidateOutboundURL(u); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid format_urls[%s]: %v", f, err))
					return
				}
			}
		}
		if err := r.store.CreateDownstream(&ds); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.requestConfigWrite()
		if ds.APIKey != "" {
			ds.APIKey = "***"
		}
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
		// Mask the API key unless the caller explicitly opts in via ?reveal=1.
		// The admin web UI uses this to populate the "Reveal" button in the
		// downstream detail pane; the endpoint is admin-only (auth-protected)
		// so revealing the key here is safe.
		if req.URL.Query().Get("reveal") != "1" && ds.APIKey != "" {
			ds.APIKey = "***"
		}
		writeJSON(w, http.StatusOK, ds)

	case http.MethodPut:
		var patch struct {
			Name           *string            `json:"name"`
			BaseURL        *string            `json:"base_url"`
			APIKey         *string            `json:"api_key"`
			ApiFormats     *[]string          `json:"api_formats"`
			OutputModelIDs *[]string          `json:"output_model_ids"`
			FormatURLs     *map[string]string `json:"format_urls"`
		}
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		existing, err := r.store.GetDownstream(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}

		// Partial update: pointer is nil → field absent from request → keep existing.
		// Pointer non-nil → overwrite (even with "" or []), so callers can clear fields.
		if patch.Name != nil {
			existing.Name = *patch.Name
		}
		if patch.BaseURL != nil {
			existing.BaseURL = *patch.BaseURL
		}
		if patch.APIKey != nil && *patch.APIKey != "" && *patch.APIKey != "***" {
			existing.APIKey = *patch.APIKey
		}
		if patch.ApiFormats != nil {
			existing.ApiFormats = *patch.ApiFormats
		}
		if patch.OutputModelIDs != nil {
			existing.OutputModelIDs = append([]string(nil), *patch.OutputModelIDs...)
		}
		if patch.FormatURLs != nil {
			// Validate every URL in the patch before committing; reject the
			// whole request if any URL is invalid.
			for f, u := range *patch.FormatURLs {
				if err := proxy.ValidateOutboundURL(u); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid format_urls[%s]: %v", f, err))
					return
				}
			}
			// Replace the whole map (not merge) so callers can clear keys by
			// omitting them — the UI builds the complete map each save.
			existing.FormatURLs = *patch.FormatURLs
		}

		if existing.BaseURL != "" {
			if err := proxy.ValidateOutboundURL(existing.BaseURL); err != nil {
				writeError(w, http.StatusBadRequest, "invalid base_url: "+err.Error())
				return
			}
		}

		if err := r.store.UpdateDownstream(existing); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.requestConfigWrite()
		if existing.APIKey != "" {
			existing.APIKey = "***"
		}
		writeJSONWithWarning(w, http.StatusOK, *existing, proxy.IsBareIP(existing.BaseURL))

	case http.MethodDelete:
		if err := r.store.DeleteDownstream(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.requestConfigWrite()
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
	r.requestConfigWrite()
	writeJSON(w, http.StatusOK, ds)
}

// handleDownstreamFetchModels handles POST /api/downstreams/{id}/fetch-models.
// It returns the list of models discovered from the upstream provider without
// auto-persisting them — callers decide which models to add via the
// per-row POST /api/downstreams/{id}/models endpoints.
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

	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"model_ids": models})
}

// fetchModels calls the downstream provider's /models endpoint to discover available models.
// It tries multiple URL patterns (/v1/models, /models) and returns specific error messages.
func (r *Router) fetchModels(ds *store.Downstream) ([]string, error) {
	// Use the per-format URL for the primary format when configured, matching
	// the engine's URL selection rule (format_urls[primary] if set, else base_url).
	baseURL := ds.BaseURL
	if ds.FormatURLs != nil && len(ds.ApiFormats) > 0 {
		if u := ds.FormatURLs[ds.ApiFormats[0]]; u != "" {
			baseURL = u
		}
	}
	return fetchModelsByCreds(baseURL, ds.APIKey, ds.ApiFormats)
}

// fetchModelsByCreds fetches models given raw credentials (used for both existing
// downstreams and the create-new-downstream form).
// apiFormats is used to choose provider-specific endpoints (e.g. Gemini).
func fetchModelsByCreds(baseURL, apiKey string, apiFormats []string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key configured — add an API key before fetching models")
	}

	baseURL = strings.TrimSuffix(baseURL, "/")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Gemini probe: GET {baseURL}{path}?key={apiKey} (or x-goog-api-key header).
	// Response shape: { "models": [ { "name": "models/gemini-2.5-pro", ... }, ... ] }.
	// The official Google Generative AI endpoint is {baseURL}/v1beta/models,
	// but providers occasionally mount it at bare /models. Probe both.
	if slices.Contains(apiFormats, "gemini") {
		geminiEndpoints := []string{baseURL + "/v1beta/models", baseURL + "/models"}
		for _, url := range geminiEndpoints {
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				continue
			}
			req.Header.Set("x-goog-api-key", apiKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// Try the next probe URL.
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return nil, fmt.Errorf("authentication failed — check the API key")
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var geminiResp struct {
					Models []struct {
						Name string `json:"name"`
					} `json:"models"`
				}
				if err := json.Unmarshal(body, &geminiResp); err == nil && len(geminiResp.Models) > 0 {
					models := make([]string, 0, len(geminiResp.Models))
					for _, m := range geminiResp.Models {
						// Strip the "models/" prefix from each entry.
						name := strings.TrimPrefix(m.Name, "models/")
						if name != "" {
							models = append(models, name)
						}
					}
					if len(models) > 0 {
						return models, nil
					}
				}
				// Body decoded but no models found — try the next URL.
				continue
			}
			// Non-2xx (e.g. 404): try the next URL.
		}
		return nil, fmt.Errorf("gemini /v1beta/models and /models both returned no models in expected format")
	}

	// Try common model endpoint patterns (OpenAI / Anthropic / generic)
	endpoints := []string{
		baseURL + "/models",   // OpenAI-style /v1/models (base_url already includes /v1)
		baseURL + "/v1/models", // Some providers need explicit /v1 prefix
	}

	// Decide which auth styles to try. A downstream declaring multiple
	// formats (e.g. minimax with [openai, anthropic]) may serve /models on
	// EITHER scheme — probe each declared format with its matching auth
	// header and succeed on the first one that returns a parseable body.
	// When apiFormats is empty, fall back to Bearer (OpenAI-compatible).
	authSchemes := buildAuthSchemeList(apiFormats)

	var lastError string

	for _, url := range endpoints {
		for _, scheme := range authSchemes {
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				continue
			}
			scheme.apply(req, apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				lastError = "unable to connect to provider"
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				// Wrong auth style for this endpoint — try the next scheme.
				lastError = "authentication failed — check the API key"
				continue
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

			// Fallback: try raw array of strings
			var strArr []string
			if err := json.Unmarshal(body, &strArr); err == nil && len(strArr) > 0 {
				return strArr, nil
			}

			lastError = "unrecognized response format"
			// Body decoded but no models found — try the next URL or scheme.
			break
		}
	}

	return nil, fmt.Errorf("%s. No working models endpoint found", lastError)
}

// authScheme describes how to attach credentials to a fetch-models probe
// request. The fetch-models probe loops over declared formats, so an
// api_formats: [openai, anthropic] downstream tries BOTH Bearer and
// x-api-key rather than hardcoding whichever the list contains.
type authScheme struct {
	name  string
	apply func(req *http.Request, apiKey string)
}

var (
	bearerAuth = authScheme{
		name: "bearer",
		apply: func(req *http.Request, apiKey string) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		},
	}
	anthropicAuth = authScheme{
		name: "anthropic",
		apply: func(req *http.Request, apiKey string) {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		},
	}
)

// buildAuthSchemeList picks the auth styles to probe, in priority order,
// based on the downstream's declared api_formats. When the list contains
// only one format we only need to try that one; when it contains several
// we try each so multi-format providers (minimax, llama-swap) don't lock
// out the format that actually serves /models. An empty list falls back
// to Bearer.
func buildAuthSchemeList(apiFormats []string) []authScheme {
	if len(apiFormats) == 0 {
		return []authScheme{bearerAuth}
	}
	seen := map[string]bool{}
	var out []authScheme
	for _, f := range apiFormats {
		switch f {
		case "openai", "openai_responses":
			if !seen["bearer"] {
				out = append(out, bearerAuth)
				seen["bearer"] = true
			}
		case "anthropic":
			if !seen["anthropic"] {
				out = append(out, anthropicAuth)
				seen["anthropic"] = true
			}
		}
	}
	if len(out) == 0 {
		return []authScheme{bearerAuth}
	}
	return out
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
		BaseURL string   `json:"base_url"`
		APIKey  string   `json:"api_key"`
		Formats []string `json:"formats,omitempty"`
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

	var formats []string
	if len(body.Formats) > 0 {
		formats = body.Formats
	}
	models, err := fetchModelsByCreds(body.BaseURL, body.APIKey, formats)
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
