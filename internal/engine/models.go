package engine

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// modelRecord is one entry in the OpenAI-compatible /v1/models listing.
type modelRecord struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Created     int64          `json:"created"`
	OwnedBy     string         `json:"owned_by"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

// handleModels responds to GET /v1/models and GET /models with an aggregated
// model list from all downstreams and aliases, formatted as an OpenAI-style
// model list response. Each entry carries downstream attribution (owned_by)
// and metadata (name, source type).
// Proxy auth is validated by HandleProxy before reaching this function.
func (e *Engine) handleModels(w http.ResponseWriter) {
	created := time.Now().Unix()
	data := make([]modelRecord, 0)

	newRecord := func(id, name, ownedBy string, source string) modelRecord {
		rec := modelRecord{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: ownedBy,
			Name:    name,
		}
		rec.Meta = map[string]any{"source": source}
		return rec
	}

	// Models from downstream output_model_ids
	downstreams, err := e.store.ListDownstreams()
	if err != nil {
		log.Printf("Error listing downstreams for models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Disabled downstreams behave "as if deleted": hide their models and any
	// alias that routes to them.
	dsName, disabled := downstreamModelIndex(downstreams)
	for _, ds := range downstreams {
		if !ds.IsEnabled {
			continue
		}
		for _, m := range ds.OutputModelIDs {
			data = append(data, newRecord(m, ds.Name, ds.ID, "downstream"))
		}
	}

	// Models from aliases (input and output)
	aliases, err := e.store.ListAliases()
	if err != nil {
		log.Printf("Error listing aliases for models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Build downstream ID -> name and disabled lookups in one pass.

	for _, a := range aliases {
		if _, off := disabled[a.DownstreamID]; off {
			continue
		}
		// Skip regex aliases for input_model_id (they represent patterns, not model IDs)
		if !a.IsRegex {
			data = append(data, newRecord(a.InputModelID, dsName[a.DownstreamID], a.DownstreamID, "alias"))
		}
		// Surface announced names from regex groups so clients see routable model IDs.
		// Announced names always win over the group's output_model_id in dedup because
		// they're emitted earlier and the dedup pass keeps the first occurrence.
		for _, n := range a.AnnouncedNames {
			data = append(data, newRecord(n, dsName[a.DownstreamID], a.DownstreamID, "alias"))
		}
		data = append(data, newRecord(a.OutputModelID, dsName[a.DownstreamID], a.DownstreamID, "alias"))
	}

	// Deduplicate: keep the first occurrence of each model ID
	seen := make(map[string]struct{}, len(data))
	deduped := make([]modelRecord, 0, len(data))
	for _, m := range data {
		if _, ok := seen[m.ID]; !ok {
			seen[m.ID] = struct{}{}
			deduped = append(deduped, m)
		}
	}

	// Sort by ID for deterministic output
	sort.Slice(deduped, func(i, j int) bool { return deduped[i].ID < deduped[j].ID })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   deduped,
	})
}

// geminiModelRecord is one entry in the Gemini-format /v1beta/models listing.
// Google's schema is documented at
// https://ai.google.dev/api/models#method:-models.list — we emit the fields
// that Gemini-format clients (e.g. Cherry Studio) actually consume, namely
// `name` (the model identifier, e.g. "models/gemini-2.5-pro") and `displayName`.
type geminiModelRecord struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName,omitempty"`
	Description                string   `json:"description,omitempty"`
	Version                    string   `json:"version,omitempty"`
	InputTokenLimit            int      `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int      `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

// handleGeminiModels responds to GET /v1beta/models with a Gemini-format list
// of available models. We surface every downstream model regardless of the
// downstream's configured `api_formats`: when a Gemini-format request comes
// in for a downstream that speaks OpenAI, Anthropic, or OpenAI Responses, the
// engine auto-inserts the appropriate Gemini->X transformer (see
// buildPipeline). Hiding those models here would be a lie about what's
// reachable.
//
// Alias inputs (the model name the client uses to talk to the gateway) are
// also surfaced so human-friendly alias names appear in the picker.
//
// Query parameters honored:
//   - pageSize:  cap on returned entries (default 1000, max 1000 to match Google's behavior)
//   - pageToken: opaque cursor returned in the previous response; we don't paginate
//     (all results fit in one page unless the catalog grows huge), so we
//     accept and ignore it but never emit one.
//
// Proxy auth is validated by HandleProxy before reaching this function.
func (e *Engine) handleGeminiModels(r *http.Request, w http.ResponseWriter) {
	const maxPageSize = 1000
	const defaultPageSize = 1000
	pageSize := defaultPageSize
	if ps := r.URL.Query().Get("pageSize"); ps != "" {
		if n, err := strconv.Atoi(ps); err == nil && n > 0 {
			if n > maxPageSize {
				n = maxPageSize
			}
			pageSize = n
		}
	}

	downstreams, err := e.store.ListDownstreams()
	if err != nil {
		log.Printf("Error listing downstreams for gemini models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	aliases, err := e.store.ListAliases()
	if err != nil {
		log.Printf("Error listing aliases for gemini models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Build a downstream-id -> name map so we can attribute models to their
	// upstream in the description field.
	dsName := make(map[string]string, len(downstreams))
	disabled := make(map[string]struct{}, len(downstreams))
	for _, ds := range downstreams {
		dsName[ds.ID] = ds.Name
		if !ds.IsEnabled {
			disabled[ds.ID] = struct{}{}
		}
	}

	geminiMethods := []string{"generateContent", "streamGenerateContent", "countTokens"}
	seen := make(map[string]struct{})
	out := make([]geminiModelRecord, 0)

	// Surface every model that any downstream advertises. Google's convention
	// is for `name` to be prefixed with "models/", so we add the prefix if
	// the downstream's output_model_id doesn't already include it. This
	// matches what Google's own /v1beta/models returns and what Cherry
	// Studio's parser (listModels.ts line 192) handles via
	// `m.name.startsWith('models/') ? m.name.slice(7) : m.name`.
	//
	// We include models from every downstream — not just Gemini-format ones
	// — because the engine can auto-translate Gemini->OpenAI/Anthropic/
	// OpenAI Responses. See the function-level comment for the rationale.
	for _, ds := range downstreams {
		if !ds.IsEnabled {
			continue
		}
		for _, m := range ds.OutputModelIDs {
			name := m
			if !strings.HasPrefix(name, "models/") {
				name = "models/" + name
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, geminiModelRecord{
				Name:                       name,
				DisplayName:                m,
				Description:                "via " + ds.Name,
				SupportedGenerationMethods: geminiMethods,
			})
		}
	}

	// Also surface alias inputs so users see their human-friendly alias
	// names in the model picker rather than only the upstream IDs. We include
	// aliases regardless of the downstream's api_formats for the same reason
	// as above (auto-translation makes any of them reachable).
	for _, a := range aliases {
		if _, off := disabled[a.DownstreamID]; off {
			continue
		}
		if a.IsRegex {
			// Skip the regex pattern itself — it's not a concrete model ID.
			// Announced names (if any) are surfaced below.
		} else {
			ds, err := e.store.GetDownstream(a.DownstreamID)
			if err != nil || ds == nil {
				continue
			}
			name := "models/" + a.InputModelID
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, geminiModelRecord{
					Name:                       name,
					DisplayName:                a.InputModelID,
					Description:                "via " + dsName[ds.ID] + " (alias for " + a.OutputModelID + ")",
					SupportedGenerationMethods: geminiMethods,
				})
			}
		}
		// Surface announced names from regex groups. These are concrete IDs
		// the user wants clients to see in the picker.
		ds, err := e.store.GetDownstream(a.DownstreamID)
		if err != nil || ds == nil {
			continue
		}
		for _, n := range a.AnnouncedNames {
			name := "models/" + n
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, geminiModelRecord{
				Name:                       name,
				DisplayName:                n,
				Description:                "via " + dsName[ds.ID] + " (alias for " + a.OutputModelID + ")",
				SupportedGenerationMethods: geminiMethods,
			})
		}
	}

	// Sort by name for deterministic output.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// Apply pageSize cap.
	if len(out) > pageSize {
		out = out[:pageSize]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": out,
	})
}
